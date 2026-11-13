// Package wbdevice публикует устройство в MQTT по Wiren Board MQTT Conventions.
package wbdevice

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

const (
	// writeQueueSize — сколько записей в /on может ждать обработчика.
	writeQueueSize = 256
	// setupRetryMin — первая пауза перед повтором публикации состояния после подключения.
	setupRetryMin = time.Second
	// setupRetryMax — предельная пауза между повторами публикации состояния.
	setupRetryMax = 30 * time.Second
	// shutdownPublishFloor — сколько Shutdown ждёт подтверждения meta/error "r", даже если
	// дедлайн ctx уже исчерпан (например, его съела досылка очереди Telegram).
	shutdownPublishFloor = time.Second
)

// write — запись в /on, ожидающая обработчика.
type write struct {
	controlID string
	payload   []byte
}

// Device — устройство в MQTT по Wiren Board MQTT Conventions.
type Device struct {
	id       string
	options  options
	client   paho.Client
	controls map[string]Control

	// stateMu упорядочивает смену поколения соединения и признака готовности.
	stateMu sync.Mutex
	// generation растёт при каждом подключении и потере связи: onConnect от старого
	// соединения по нему понимает, что устарел, и не выставляет ready.
	generation uint64
	// ready — устройство подключено, его состояние опубликовано и подписка на /on оформлена.
	ready atomic.Bool
	// firstReady закрывается при первой готовности; его ждёт Run.
	firstReady     chan struct{}
	firstReadyOnce sync.Once

	// controlLock защищает values/errors и упорядочивает публикацию состояния контролов,
	// чтобы переподключение не затёрло более свежее значение из SetValue/SetError.
	controlLock chan struct{}
	values      map[string]string
	errors      map[string]string

	writes   chan write
	stop     chan struct{}
	stopOnce sync.Once
	workers  sync.WaitGroup

	// staleOnce ограничивает уборку устаревших контролов одним разом за время жизни процесса.
	staleOnce sync.Once

	// flagMu упорядочивает постановку в очередь флага ошибки устройства: "" в конце setup и "r"
	// в Shutdown. Пустой флаг от устаревшего соединения не может уйти после "r".
	flagMu sync.Mutex
}

// New создаёт устройство deviceID; подключение — в Run.
func New(deviceID string, opts ...Option) (*Device, error) {
	o := defaultOptions()

	for _, opt := range opts {
		opt(&o)
	}

	if !identifierPattern.MatchString(deviceID) {
		return nil, &Error{kind: kindInvalidArgument, Op: "new", Message: "bad device id " + deviceID}
	}

	errValidate := o.Validate()
	if errValidate != nil {
		return nil, errValidate
	}

	d := &Device{
		id:          deviceID,
		options:     o,
		controls:    map[string]Control{},
		values:      map[string]string{},
		errors:      map[string]string{},
		writes:      make(chan write, writeQueueSize),
		stop:        make(chan struct{}),
		controlLock: make(chan struct{}, 1),

		firstReady: make(chan struct{}),
	}

	for _, control := range o.controls {
		d.controls[control.ID] = control
		d.values[control.ID] = control.Initial
		d.errors[control.ID] = ""
	}

	clientOptions := paho.NewClientOptions().
		AddBroker(o.broker).
		SetClientID(o.clientID).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetWriteTimeout(min(o.timeout, time.Second)).
		SetStore(discardStore{}).
		SetWill(deviceErrorTopic(deviceID), ErrorFlagRead, 1, true).
		SetOnConnectHandler(d.onConnect).
		SetConnectionLostHandler(d.onConnectionLost).
		SetReconnectingHandler(d.onReconnecting)

	d.client = paho.NewClient(clientOptions)

	return d, nil
}

// Run запускает доставку записей обработчику, подключается к брокеру и ждёт (не дольше таймаута
// устройства), пока устройство будет опубликовано и начнёт принимать записи в /on: компоненты,
// запущенные после него, застают устройство готовым. Если брокер пока недоступен или публикация
// затянулась, подключение продолжается в фоне, а Connected остаётся false до готовности.
func (d *Device) Run(ctx context.Context) error {
	d.workers.Add(1)

	go d.dispatchWrites()

	deadline := time.NewTimer(d.options.timeout)
	defer deadline.Stop()

	token := d.client.Connect()

	select {
	case <-token.Done():
	case <-ctx.Done():
		return nil
	case <-deadline.C:
		slog.Warn("MQTT: брокер пока недоступен, подключение продолжится в фоне", "broker", d.options.broker)

		return nil
	}

	if token.Error() != nil {
		return &Error{kind: kindMQTT, Op: "connect", Message: token.Error().Error()}
	}

	select {
	case <-d.firstReady:
	case <-d.stop:
	case <-ctx.Done():
	case <-deadline.C:
		slog.Warn("MQTT: устройство ещё не опубликовано, продолжаю в фоне", "device", d.id)
	}

	return nil
}

// onConnect при каждом (пере)подключении публикует состояние устройства и подписывается на /on,
// повторяя попытки с нарастающей паузой, пока соединение то же самое и устройство не остановлено.
func (d *Device) onConnect(_ paho.Client) {
	generation := d.nextGeneration()
	delay := setupRetryMin

	for {
		errSetup := d.setup(generation)
		if errSetup == nil {
			break
		}

		// Остановка или новое подключение во время публикации — не ошибка.
		if d.isStopped() || !d.isCurrent(generation) {
			return
		}

		slog.Error("MQTT: не удалось опубликовать устройство, повторю", "err", errSetup, "delay", delay)

		select {
		case <-d.stop:
			return
		case <-time.After(delay):
		}

		if !d.isCurrent(generation) || !d.client.IsConnectionOpen() {
			return
		}

		delay = min(delay*2, setupRetryMax)
	}

	d.markReady(generation)
}

// setup публикует состояние устройства, один раз за процесс убирает контролы удалённых команд,
// подписывается на запись в контролы и последним шагом снимает ошибку устройства — всё, что должно
// быть сделано до того, как устройство считается готовым. Пустой meta/error означает для UI и правил,
// что устройство работает, поэтому он публикуется только когда записи в /on уже принимаются.
//
// Уборка идёт до подписки на /on: её подписка на controls/# перекрывала бы подписки на /on,
// и запись в контрол могла бы дойти до обработчика дважды.
func (d *Device) setup(generation uint64) error {
	errPublish := d.publishState()
	if errPublish != nil {
		return errPublish
	}

	d.staleOnce.Do(d.removeStale)

	errSubscribe := d.subscribeWrites()
	if errSubscribe != nil {
		return errSubscribe
	}

	return d.publishHealthy(generation)
}

// publishHealthy снимает флаг ошибки устройства, если соединение generation всё ещё текущее.
// Постановка в очередь идёт под flagMu, как и "r" в Shutdown, а paho отправляет сообщения
// в порядке постановки: после остановки пустой флаг не перезапишет "r".
func (d *Device) publishHealthy(generation uint64) error {
	d.flagMu.Lock()

	if !d.isCurrent(generation) {
		d.flagMu.Unlock()

		return nil
	}

	token, errEnqueue := d.enqueue(deviceErrorTopic(d.id), "")

	d.flagMu.Unlock()

	if errEnqueue != nil {
		return errEnqueue
	}

	return waitPublished(token, deviceErrorTopic(d.id), d.options.timeout)
}

// onConnectionLost записывает причину потери связи.
func (d *Device) onConnectionLost(_ paho.Client, err error) {
	slog.Warn("MQTT: соединение потеряно, переподключаюсь", "err", err)
}

// onReconnecting снимает признак готовности перед каждой попыткой переподключения.
func (d *Device) onReconnecting(_ paho.Client, _ *paho.ClientOptions) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	d.generation++
	d.ready.Store(false)

	if !d.isStopped() {
		d.options.onConnected(false)
	}
}

// nextGeneration начинает новое поколение соединения, снимает ready и возвращает номер поколения.
func (d *Device) nextGeneration() uint64 {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	d.generation++
	d.ready.Store(false)

	return d.generation
}

// isCurrent сообщает, что поколение generation всё ещё текущее.
func (d *Device) isCurrent(generation uint64) bool {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	return d.generation == generation && !d.isStopped()
}

// markReady выставляет ready, если поколение generation всё ещё текущее.
func (d *Device) markReady(generation uint64) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	if d.generation != generation || d.isStopped() || !d.client.IsConnectionOpen() {
		return
	}

	d.ready.Store(true)
	d.firstReadyOnce.Do(func() { close(d.firstReady) })

	slog.Info("MQTT: устройство опубликовано", "device", d.id)
	d.options.onConnected(true)
}

// publishState публикует meta устройства, meta, значения и ошибки контролов; ошибку устройства
// снимает setup, когда устройство готово.
func (d *Device) publishState() error {
	deviceMeta, errMarshal := json.Marshal(DeviceMeta{Driver: d.options.driver, Title: d.options.title})
	if errMarshal != nil {
		return &Error{kind: kindInvalidArgument, Op: "encode meta", Message: errMarshal.Error()}
	}

	errMeta := d.publish(deviceMetaTopic(d.id), string(deviceMeta))
	if errMeta != nil {
		return errMeta
	}

	for _, control := range d.options.controls {
		errControl := d.publishControl(control)
		if errControl != nil {
			return errControl
		}
	}

	return nil
}

// publishControl публикует meta, текущее значение и флаг ошибки контрола.
// Значение Volatile-контрола при (пере)подключении возвращается к Initial и не повторяется;
// пустое значение не публикуется (см. Control.Initial).
func (d *Device) publishControl(control Control) error {
	meta, errMarshal := json.Marshal(control.Meta)
	if errMarshal != nil {
		return &Error{kind: kindInvalidArgument, Op: "encode control meta", Message: errMarshal.Error()}
	}

	errLock := d.lockControls(context.Background())
	if errLock != nil {
		return errLock
	}

	defer d.unlockControls()

	if control.Volatile {
		d.values[control.ID] = control.Initial
	}

	errMeta := d.publish(controlMetaTopic(d.id, control.ID), string(meta))
	if errMeta != nil {
		return errMeta
	}

	if d.values[control.ID] != "" {
		errValue := d.publish(controlTopic(d.id, control.ID), d.values[control.ID])
		if errValue != nil {
			return errValue
		}
	}

	return d.publish(controlErrorTopic(d.id, control.ID), d.errors[control.ID])
}

// subscribeWrites подписывается на /on всех записываемых контролов.
func (d *Device) subscribeWrites() error {
	for _, control := range d.options.controls {
		if control.Meta.Readonly {
			continue
		}

		topic := controlOnTopic(d.id, control.ID)

		token := d.client.Subscribe(topic, 1, d.handleWrite)

		errSubscribe := waitSubscribed(token, topic, d.options.timeout)
		if errSubscribe != nil {
			return errSubscribe
		}
	}

	return nil
}

// waitSubscribed проверяет подтверждение подписки от брокера.
func waitSubscribed(token paho.Token, topic string, timeout time.Duration) error {
	if !token.WaitTimeout(timeout) {
		return &Error{kind: kindTimeout, Op: "subscribe", Topic: topic, Message: "no answer from broker"}
	}

	if errSubscribe := token.Error(); errSubscribe != nil {
		return &Error{kind: kindMQTT, Op: "subscribe", Topic: topic, Message: errSubscribe.Error()}
	}

	subscription, ok := token.(*paho.SubscribeToken)
	if !ok {
		return &Error{kind: kindMQTT, Op: "subscribe", Topic: topic, Message: "unexpected subscription token"}
	}

	qos, granted := subscription.Result()[topic]
	if !granted || qos > 2 {
		return &Error{kind: kindMQTT, Op: "subscribe", Topic: topic, Message: "broker denied subscription"}
	}

	return nil
}

// handleWrite принимает свежую запись в /on без ожидания места в очереди.
func (d *Device) handleWrite(_ paho.Client, message paho.Message) {
	if message.Retained() || d.isStopped() {
		return
	}

	controlID, ok := controlIDFromTopic(d.id, message.Topic())
	if !ok || d.options.onWrite == nil {
		return
	}

	item := write{controlID: controlID, payload: append([]byte(nil), message.Payload()...)}

	select {
	case d.writes <- item:
	default:
		slog.Warn("MQTT: очередь записей заполнена, новая запись пропущена", "control", controlID)
	}
}

// dispatchWrites по порядку передаёт записи из очереди обработчику до остановки устройства.
func (d *Device) dispatchWrites() {
	defer d.workers.Done()

	for {
		select {
		case <-d.stop:
			return
		case item := <-d.writes:
			d.options.onWrite(item.controlID, item.payload)
		}
	}
}

// SetValue публикует значение контрола и обновляет локальное состояние для переподключений.
func (d *Device) SetValue(ctx context.Context, controlID, value string) error {
	if _, ok := d.controls[controlID]; !ok {
		return &Error{kind: kindUnknownControl, Op: "set value", Message: controlID}
	}

	errLock := d.lockControls(ctx)
	if errLock != nil {
		return errLock
	}

	defer d.unlockControls()

	d.values[controlID] = value

	errPublish := d.publishControlValue(ctx, controlTopic(d.id, controlID), value)
	if errPublish != nil {
		d.values[controlID] = d.controls[controlID].Initial
	}

	return errPublish
}

// SetError публикует флаги ошибки контрола ("" — ошибки нет).
func (d *Device) SetError(ctx context.Context, controlID, flags string) error {
	if _, ok := d.controls[controlID]; !ok {
		return &Error{kind: kindUnknownControl, Op: "set error", Message: controlID}
	}

	errLock := d.lockControls(ctx)
	if errLock != nil {
		return errLock
	}

	defer d.unlockControls()

	d.errors[controlID] = flags

	return d.publishControlValue(ctx, controlErrorTopic(d.id, controlID), flags)
}

// lockControls резервирует публикацию состояния контрола.
func (d *Device) lockControls(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.stop:
		return ErrNotConnected
	case d.controlLock <- struct{}{}:
	}

	if errCtx := ctx.Err(); errCtx != nil {
		d.unlockControls()

		return errCtx
	}

	if d.isStopped() {
		d.unlockControls()

		return ErrNotConnected
	}

	return nil
}

// unlockControls освобождает публикацию состояния контрола.
func (d *Device) unlockControls() {
	<-d.controlLock
}

// publishControlValue публикует состояние контрола с отменой ожидания PUBACK.
func (d *Device) publishControlValue(ctx context.Context, topic, value string) error {
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}

	token, errEnqueue := d.enqueue(topic, value)
	if errEnqueue != nil {
		return errEnqueue
	}

	timer := time.NewTimer(d.options.timeout)
	defer timer.Stop()

	select {
	case <-token.Done():
		return publishResult(token, topic)
	case <-timer.C:
	case <-ctx.Done():
	case <-d.stop:
	}

	select {
	case <-token.Done():
		return publishResult(token, topic)
	default:
		return &Error{kind: kindTimeout, Op: "publish", Topic: topic, Message: "publication not confirmed"}
	}
}

// isStopped сообщает, что устройство останавливается.
func (d *Device) isStopped() bool {
	select {
	case <-d.stop:
		return true
	default:
		return false
	}
}

// Connected сообщает, что устройство подключено к брокеру, опубликовано и принимает записи в /on.
func (d *Device) Connected() bool {
	return d.ready.Load() && d.client.IsConnectionOpen()
}

// Shutdown помечает устройство ошибкой r, отключается от брокера и дожидается остановки доставки
// записей. Подтверждение r ждётся не дольше таймаута устройства и дедлайна ctx, но не меньше
// shutdownPublishFloor: даже с исчерпанным ctx устройство пытается честно сказать, что остановлено.
// Вызывать из WriteHandler нельзя.
func (d *Device) Shutdown(ctx context.Context) error {
	d.stopOnce.Do(func() { close(d.stop) })

	timeout := d.options.timeout

	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, max(time.Until(deadline), shutdownPublishFloor))
	}

	var errPublish error

	d.flagMu.Lock()

	d.nextGeneration()

	token, errEnqueue := d.enqueue(deviceErrorTopic(d.id), ErrorFlagRead)

	d.flagMu.Unlock()

	if errEnqueue == nil {
		errPublish = waitPublished(token, deviceErrorTopic(d.id), timeout)
	}

	d.client.Disconnect(250)
	d.workers.Wait()

	return errPublish
}

// Stop останавливает устройство так же, как Shutdown.
func (d *Device) Stop() error {
	return d.Shutdown(context.Background())
}

// publish публикует retained-сообщение с QoS 1 и ждёт подтверждения не дольше таймаута устройства.
func (d *Device) publish(topic, payload string) error {
	return d.publishWithin(topic, payload, d.options.timeout)
}

// publishWithin публикует retained-сообщение с QoS 1 и ждёт подтверждения не дольше timeout.
func (d *Device) publishWithin(topic, payload string, timeout time.Duration) error {
	token, errEnqueue := d.enqueue(topic, payload)
	if errEnqueue != nil {
		return errEnqueue
	}

	return waitPublished(token, topic, timeout)
}

// enqueue ставит retained-сообщение с QoS 1 в очередь отправки paho.
func (d *Device) enqueue(topic, payload string) (paho.Token, error) {
	if !d.client.IsConnectionOpen() {
		return nil, &Error{kind: kindNotConnected, Op: "publish", Topic: topic, Message: "no connection to broker"}
	}

	return d.client.Publish(topic, 1, true, payload), nil
}

// waitPublished ждёт подтверждения публикации не дольше timeout.
func waitPublished(token paho.Token, topic string, timeout time.Duration) error {
	if !token.WaitTimeout(timeout) {
		return &Error{kind: kindTimeout, Op: "publish", Topic: topic, Message: "no answer from broker"}
	}

	return publishResult(token, topic)
}

// publishResult возвращает результат завершённого токена публикации.
func publishResult(token paho.Token, topic string) error {
	if errPublish := token.Error(); errPublish != nil {
		return &Error{kind: kindMQTT, Op: "publish", Topic: topic, Message: errPublish.Error()}
	}

	return nil
}

// errorKind классифицирует ошибки пакета wbdevice.
type errorKind int

const (
	kindUnknown errorKind = iota
	kindNotConnected
	kindUnknownControl
	kindInvalidArgument
	kindTimeout
	kindMQTT
)

// Error — ошибка пакета wbdevice.
type Error struct {
	kind    errorKind
	Op      string
	Topic   string
	Message string
}

// Error возвращает текст ошибки.
func (e *Error) Error() string {
	text := "wbdevice"

	if e.Op != "" {
		text += ": " + e.Op
	}

	if e.Topic != "" {
		text += " " + e.Topic
	}

	return text + ": " + e.Message
}

// Is сравнивает ошибки по виду.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}

	return e.kind == t.kind
}

var (
	// ErrNotConnected сообщает, что соединения с брокером нет.
	ErrNotConnected = &Error{kind: kindNotConnected, Message: "not connected"}
	// ErrUnknownControl сообщает об обращении к незарегистрированному контролу.
	ErrUnknownControl = &Error{kind: kindUnknownControl, Message: "unknown control"}
	// ErrInvalidArgument сообщает о некорректном идентификаторе или опции.
	ErrInvalidArgument = &Error{kind: kindInvalidArgument, Message: "invalid argument"}
	// ErrTimeout сообщает, что брокер не подтвердил начатую операцию.
	ErrTimeout = &Error{kind: kindTimeout, Message: "timeout"}
	// ErrMQTT сообщает об ошибке операции MQTT.
	ErrMQTT = &Error{kind: kindMQTT, Message: "mqtt error"}
)
