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

	// mu защищает values/errors и упорядочивает публикацию состояния контролов,
	// чтобы переподключение не затёрло более свежее значение из SetValue/SetError.
	mu     sync.Mutex
	values map[string]string
	errors map[string]string

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
		id:       deviceID,
		options:  o,
		controls: map[string]Control{},
		values:   map[string]string{},
		errors:   map[string]string{},
		writes:   make(chan write, writeQueueSize),
		stop:     make(chan struct{}),

		firstReady: make(chan struct{}),
	}

	for _, control := range o.controls {
		d.controls[control.ID] = control
		d.values[control.ID] = ""
		d.errors[control.ID] = ""
	}

	clientOptions := paho.NewClientOptions().
		AddBroker(o.broker).
		SetClientID(o.clientID).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
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

	if d.markReady(generation) {
		slog.Info("MQTT: устройство опубликовано", "device", d.id)
	}
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

// onConnectionLost снимает признак готовности; переподключение paho выполняет сам.
func (d *Device) onConnectionLost(_ paho.Client, err error) {
	d.nextGeneration()

	slog.Warn("MQTT: соединение потеряно, переподключаюсь", "err", err)
}

// onReconnecting снимает признак готовности перед каждой попыткой переподключения.
func (d *Device) onReconnecting(_ paho.Client, _ *paho.ClientOptions) {
	d.nextGeneration()
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

	return d.generation == generation
}

// markReady выставляет ready, если поколение generation всё ещё текущее.
func (d *Device) markReady(generation uint64) bool {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	if d.generation != generation {
		return false
	}

	d.ready.Store(true)
	d.firstReadyOnce.Do(func() { close(d.firstReady) })

	return true
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
// Значение Volatile-контрола при (пере)подключении сбрасывается в "" и не повторяется.
func (d *Device) publishControl(control Control) error {
	meta, errMarshal := json.Marshal(control.Meta)
	if errMarshal != nil {
		return &Error{kind: kindInvalidArgument, Op: "encode control meta", Message: errMarshal.Error()}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if control.Volatile {
		d.values[control.ID] = ""
	}

	errMeta := d.publish(controlMetaTopic(d.id, control.ID), string(meta))
	if errMeta != nil {
		return errMeta
	}

	errValue := d.publish(controlTopic(d.id, control.ID), d.values[control.ID])
	if errValue != nil {
		return errValue
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
		if !token.WaitTimeout(d.options.timeout) {
			return &Error{kind: kindTimeout, Op: "subscribe", Topic: topic, Message: "no answer from broker"}
		}

		if token.Error() != nil {
			return &Error{kind: kindMQTT, Op: "subscribe", Topic: topic, Message: token.Error().Error()}
		}
	}

	return nil
}

// handleWrite ставит запись в /on в очередь обработчика, не блокируя маршрутизатор paho
// (иначе обработчик, вызывающий SetValue, ждал бы PUBACK, который некому принять).
func (d *Device) handleWrite(_ paho.Client, message paho.Message) {
	controlID, ok := controlIDFromTopic(d.id, message.Topic())
	if !ok || d.options.onWrite == nil {
		return
	}

	item := write{controlID: controlID, payload: append([]byte(nil), message.Payload()...)}

	select {
	case d.writes <- item:
	case <-d.stop:
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

// SetValue публикует новое значение контрола (retained) и запоминает его для переподключений.
// Если опубликовать не удалось, значение сбрасывается в "", чтобы команда, о неудаче
// которой уже сообщили, не выполнилась позже при переподключении.
func (d *Device) SetValue(controlID, value string) error {
	if _, ok := d.controls[controlID]; !ok {
		return &Error{kind: kindUnknownControl, Op: "set value", Message: controlID}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.values[controlID] = value

	errPublish := d.publish(controlTopic(d.id, controlID), value)
	if errPublish != nil {
		d.values[controlID] = ""
	}

	return errPublish
}

// SetError публикует флаги ошибки контрола ("" — ошибки нет).
func (d *Device) SetError(controlID, flags string) error {
	if _, ok := d.controls[controlID]; !ok {
		return &Error{kind: kindUnknownControl, Op: "set error", Message: controlID}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.errors[controlID] = flags

	return d.publish(controlErrorTopic(d.id, controlID), flags)
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

	if token.Error() != nil {
		return &Error{kind: kindMQTT, Op: "publish", Topic: topic, Message: token.Error().Error()}
	}

	return nil
}
