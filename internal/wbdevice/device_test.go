package wbdevice

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

// testControls возвращает набор контролов, как у бота: send и одна команда.
func testControls() []Control {
	return []Control{
		{ID: "send", Meta: ControlMeta{Type: ControlTypeText, Title: map[string]string{"en": "Message", "ru": "Сообщение"}, Order: 1}},
		{ID: "cmd_gate", Meta: ControlMeta{Type: ControlTypeText, Title: map[string]string{"en": "/gate", "ru": "Ворота"}, Order: 2, Readonly: true}, Volatile: true},
	}
}

// startDevice создаёт и запускает устройство на тестовом брокере.
func startDevice(t *testing.T, broker string, handler WriteHandler, controls ...Control) *Device {
	t.Helper()

	dev, errNew := New("telegram_bot",
		WithBroker(broker),
		WithClientID("dev-"+t.Name()),
		WithTitle("Telegram Bot", "Telegram-бот"),
		WithControls(controls...),
		WithWriteHandler(handler),
		WithStaleWindow(200*time.Millisecond),
	)
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	errRun := dev.Run(context.Background())
	if errRun != nil {
		t.Fatalf("Run: %v", errRun)
	}

	waitFor(t, "device connected", dev.Connected)
	t.Cleanup(func() { _ = dev.Stop() })

	return dev
}

// writeRecorder запоминает записи, полученные обработчиком WriteHandler.
type writeRecorder struct {
	mu  sync.Mutex
	got []string
}

// handle — WriteHandler, сохраняющий запись в виде "control=payload".
func (r *writeRecorder) handle(controlID string, payload []byte) {
	r.mu.Lock()
	r.got = append(r.got, controlID+"="+string(payload))
	r.mu.Unlock()
}

// writes возвращает копию полученных записей.
func (r *writeRecorder) writes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.got...)
}

// TestDevicePublishesInitialState проверяет meta, пустые значения и снятие ошибки при подключении.
func TestDevicePublishesInitialState(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)
	obs := newObserver(t, broker, "/devices/telegram_bot/#")

	obs.publish(t, "/devices/telegram_bot/controls/cmd_gate", `{"id":"old"}`, true)
	startDevice(t, broker, nil, testControls()...)

	waitFor(t, "device meta", func() bool {
		_, ok := obs.get("/devices/telegram_bot/meta")

		return ok
	})

	meta, _ := obs.get("/devices/telegram_bot/meta")

	var gotMeta map[string]any

	_ = json.Unmarshal([]byte(meta), &gotMeta)

	title, _ := gotMeta["title"].(map[string]any)
	if gotMeta["driver"] != "wb-telegram" || title["ru"] != "Telegram-бот" {
		t.Fatalf("device meta = %s", meta)
	}

	waitFor(t, "control meta", func() bool {
		value, ok := obs.get("/devices/telegram_bot/controls/cmd_gate/meta")

		return ok && value == `{"type":"text","title":{"en":"/gate","ru":"Ворота"},"order":2,"readonly":true}`
	})

	waitFor(t, "stale command value cleared", func() bool {
		value, ok := obs.get("/devices/telegram_bot/controls/cmd_gate")

		return ok && value == ""
	})

	waitFor(t, "device error cleared", func() bool {
		value, ok := obs.get("/devices/telegram_bot/meta/error")

		return ok && value == ""
	})
}

// TestDeviceWriteHandler проверяет доставку записей в /on только для записываемых контролов.
func TestDeviceWriteHandler(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)
	recorder := &writeRecorder{}

	startDevice(t, broker, recorder.handle, testControls()...)

	obs := newObserver(t, broker, "/devices/telegram_bot/#")
	obs.publish(t, "/devices/telegram_bot/controls/cmd_gate/on", "hack", false)
	obs.publish(t, "/devices/telegram_bot/controls/send/on", "Привет", false)

	waitFor(t, "write delivered", func() bool {
		return len(recorder.writes()) > 0
	})

	time.Sleep(200 * time.Millisecond)

	got := recorder.writes()
	if len(got) != 1 || got[0] != "send=Привет" {
		t.Fatalf("writes = %v", got)
	}
}

// TestDeviceSetValueAndError проверяет публикацию значения и флага ошибки контрола.
func TestDeviceSetValueAndError(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)
	dev := startDevice(t, broker, nil, testControls()...)
	obs := newObserver(t, broker, "/devices/telegram_bot/#")

	waitForRetained(t, obs, "/devices/telegram_bot/meta")

	errValue := dev.SetValue("cmd_gate", `{"id":"1"}`)
	if errValue != nil {
		t.Fatalf("SetValue: %v", errValue)
	}

	errError := dev.SetError("send", ErrorFlagWrite)
	if errError != nil {
		t.Fatalf("SetError: %v", errError)
	}

	waitFor(t, "value and error", func() bool {
		value, _ := obs.get("/devices/telegram_bot/controls/cmd_gate")
		flags, _ := obs.get("/devices/telegram_bot/controls/send/meta/error")

		return value == `{"id":"1"}` && flags == "w"
	})

	errUnknown := dev.SetValue("nope", "x")
	if !errors.Is(errUnknown, ErrUnknownControl) {
		t.Fatalf("SetValue(nope) = %v, want ErrUnknownControl", errUnknown)
	}
}

// TestDeviceReconnect проверяет, что после обрыва связи устройство заново публикует состояние и подписку на /on:
// обычный контрол — с последним значением, Volatile-контрол (вызов команды) — пустым, чтобы вызов не повторился.
func TestDeviceReconnect(t *testing.T) {
	t.Parallel()

	srv, broker := startBrokerServer(t)
	recorder := &writeRecorder{}
	dev := startDevice(t, broker, recorder.handle, testControls()...)
	obs := newObserver(t, broker, "/devices/telegram_bot/#")

	waitForRetained(t, obs, "/devices/telegram_bot/meta")

	errValue := dev.SetValue("cmd_gate", `{"id":"7"}`)
	if errValue != nil {
		t.Fatalf("SetValue: %v", errValue)
	}

	errSend := dev.SetValue("send", "привет")
	if errSend != nil {
		t.Fatalf("SetValue send: %v", errSend)
	}

	waitFor(t, "values before reconnect", func() bool {
		value, _ := obs.get("/devices/telegram_bot/controls/cmd_gate")
		text, _ := obs.get("/devices/telegram_bot/controls/send")

		return value == `{"id":"7"}` && text == "привет"
	})

	metaCount := obs.count("/devices/telegram_bot/meta")
	errorCount := obs.count("/devices/telegram_bot/meta/error")
	valueCount := obs.count("/devices/telegram_bot/controls/cmd_gate")
	sendCount := obs.count("/devices/telegram_bot/controls/send")

	client, ok := srv.Clients.Get("dev-" + t.Name())
	if !ok {
		t.Fatal("device client not found on broker")
	}

	client.Stop(errors.New("test: connection dropped"))

	waitFor(t, "device meta republished", func() bool {
		return obs.count("/devices/telegram_bot/meta") > metaCount
	})

	waitFor(t, "device error cleared again", func() bool {
		value, _ := obs.get("/devices/telegram_bot/meta/error")

		return obs.count("/devices/telegram_bot/meta/error") > errorCount && value == ""
	})

	waitFor(t, "command value republished empty", func() bool {
		value, _ := obs.get("/devices/telegram_bot/controls/cmd_gate")

		return obs.count("/devices/telegram_bot/controls/cmd_gate") > valueCount && value == ""
	})

	waitFor(t, "send value republished", func() bool {
		text, _ := obs.get("/devices/telegram_bot/controls/send")

		return obs.count("/devices/telegram_bot/controls/send") > sendCount && text == "привет"
	})

	waitFor(t, "device ready after reconnect", dev.Connected)

	obs.publish(t, "/devices/telegram_bot/controls/send/on", "после переподключения", false)

	waitFor(t, "write after reconnect", func() bool {
		got := recorder.writes()

		return len(got) == 1 && got[0] == "send=после переподключения"
	})
}

// TestDeviceHandlerCanSetValue проверяет, что обработчик записи может вызывать SetValue
// без блокировки приёма подтверждений от брокера, даже когда записи идут подряд.
func TestDeviceHandlerCanSetValue(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)

	var (
		current  atomic.Pointer[Device]
		handled  atomic.Int64
		failures atomic.Int64
	)

	handler := func(controlID string, payload []byte) {
		errSet := current.Load().SetValue(controlID, string(payload))
		if errSet != nil {
			failures.Add(1)
		}

		handled.Add(1)
	}

	dev, errNew := New("telegram_bot", WithBroker(broker), WithClientID("dev-"+t.Name()),
		WithControls(testControls()...), WithWriteHandler(handler))
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	current.Store(dev)

	errRun := dev.Run(context.Background())
	if errRun != nil {
		t.Fatalf("Run: %v", errRun)
	}

	t.Cleanup(func() { _ = dev.Stop() })

	waitFor(t, "device connected", dev.Connected)

	obs := newObserver(t, broker, "/devices/telegram_bot/controls/send/on")
	started := time.Now()

	const total = 10

	tokens := make([]paho.Token, 0, total)

	for i := range total {
		tokens = append(tokens, obs.client.Publish("/devices/telegram_bot/controls/send/on", 1, false, "msg-"+strconv.Itoa(i)))
	}

	for _, token := range tokens {
		if !token.WaitTimeout(5*time.Second) || token.Error() != nil {
			t.Fatalf("publish: %v", token.Error())
		}
	}

	waitFor(t, "all writes handled", func() bool {
		return handled.Load() == total
	})

	// Без отдельной горутины обработчика каждый SetValue ждал бы таймаут 10 с.
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("writes took %v", elapsed)
	}

	if failures.Load() != 0 {
		t.Fatalf("SetValue failed %d times", failures.Load())
	}
}

// TestDeviceSetValueFailureNotReplayed проверяет, что значение, которое не удалось опубликовать,
// не публикуется позже при подключении: о неудаче команды уже сообщили.
func TestDeviceSetValueFailureNotReplayed(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)
	obs := newObserver(t, broker, "/devices/telegram_bot/#")

	dev, errNew := New("telegram_bot", WithBroker(broker), WithClientID("dev-"+t.Name()), WithControls(testControls()...))
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	t.Cleanup(func() { _ = dev.Stop() })

	errValue := dev.SetValue("cmd_gate", `{"id":"failed"}`)
	if !errors.Is(errValue, ErrNotConnected) {
		t.Fatalf("SetValue before connect = %v, want ErrNotConnected", errValue)
	}

	errRun := dev.Run(context.Background())
	if errRun != nil {
		t.Fatalf("Run: %v", errRun)
	}

	waitFor(t, "device connected", dev.Connected)
	waitFor(t, "command value published", func() bool {
		return obs.count("/devices/telegram_bot/controls/cmd_gate") > 0
	})

	if value, _ := obs.get("/devices/telegram_bot/controls/cmd_gate"); value != "" {
		t.Fatalf("value after connect = %q, want empty", value)
	}
}

// rejectOnceHook — хук брокера, один раз молча отбрасывающий публикацию в topic (без PUBACK).
type rejectOnceHook struct {
	mqtt.HookBase
	topic    string
	rejected atomic.Bool
}

// ID возвращает имя хука.
func (h *rejectOnceHook) ID() string {
	return "reject-once"
}

// Provides сообщает, что хук обрабатывает OnPublish.
func (h *rejectOnceHook) Provides(b byte) bool {
	return b == mqtt.OnPublish
}

// OnPublish отбрасывает первую публикацию в topic.
func (h *rejectOnceHook) OnPublish(_ *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	if pk.TopicName == h.topic && h.rejected.CompareAndSwap(false, true) {
		return pk, packets.ErrRejectPacket
	}

	return pk, nil
}

// TestDeviceRetriesSetup проверяет повтор публикации состояния, если брокер не подтвердил её,
// а соединение осталось открытым.
func TestDeviceRetriesSetup(t *testing.T) {
	t.Parallel()

	hook := &rejectOnceHook{topic: "/devices/telegram_bot/meta/error"}
	_, broker := startBrokerServer(t, hook)
	obs := newObserver(t, broker, "/devices/telegram_bot/meta/error")

	dev, errNew := New("telegram_bot", WithBroker(broker), WithClientID("dev-"+t.Name()),
		WithControls(testControls()...), WithTimeout(200*time.Millisecond))
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	t.Cleanup(func() { _ = dev.Stop() })

	errRun := dev.Run(context.Background())
	if errRun != nil {
		t.Fatalf("Run: %v", errRun)
	}

	waitFor(t, "device ready after retry", dev.Connected)

	if !hook.rejected.Load() {
		t.Fatal("first meta/error publish was not rejected")
	}

	waitFor(t, "device error cleared", func() bool {
		value, ok := obs.get("/devices/telegram_bot/meta/error")

		return ok && value == ""
	})
}

// TestDeviceShutdownHonorsContext проверяет, что Shutdown не ждёт брокер дольше дедлайна ctx
// (с учётом минимума shutdownPublishFloor на публикацию r).
func TestDeviceShutdownHonorsContext(t *testing.T) {
	t.Parallel()

	hook := &rejectOnceHook{topic: "/devices/telegram_bot/meta/error"}
	srv, broker := startBrokerServer(t)
	dev := startDevice(t, broker, nil, testControls()...)

	errHook := srv.AddHook(hook, nil)
	if errHook != nil {
		t.Fatalf("AddHook: %v", errHook)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()

	errShutdown := dev.Shutdown(ctx)
	if !errors.Is(errShutdown, ErrTimeout) {
		t.Fatalf("Shutdown = %v, want ErrTimeout", errShutdown)
	}

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %v", elapsed)
	}
}

// TestDeviceWill проверяет, что устройство регистрирует LWT: retained meta/error = r.
// Проверяется will из пакета CONNECT, принятого брокером, — так тест не зависит от того,
// как и когда брокер обрабатывает обрыв.
func TestDeviceWill(t *testing.T) {
	t.Parallel()

	srv, broker := startBrokerServer(t)
	startDevice(t, broker, nil, testControls()...)

	client, ok := srv.Clients.Get("dev-" + t.Name())
	if !ok {
		t.Fatal("device client not found on broker")
	}

	will := client.Properties.Will
	if will.TopicName != "/devices/telegram_bot/meta/error" || string(will.Payload) != ErrorFlagRead ||
		!will.Retain || will.Qos != 1 {
		t.Fatalf("will = topic %q payload %q retain %v qos %d", will.TopicName, will.Payload, will.Retain, will.Qos)
	}
}

// TestDeviceRunWaitsReady проверяет, что Run возвращается уже готовым устройством и что пустой
// meta/error (сигнал «устройство работает») публикуется только когда записи в /on уже принимаются:
// запись, отправленная сразу по этому сигналу, доходит до обработчика.
func TestDeviceRunWaitsReady(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)
	recorder := &writeRecorder{}
	obs := newObserver(t, broker, seedFilter)

	cleared := make(chan struct{})

	var clearedOnce sync.Once

	sub := obs.client.Subscribe("/devices/telegram_bot/meta/error", 1, func(_ paho.Client, m paho.Message) {
		if len(m.Payload()) == 0 {
			clearedOnce.Do(func() { close(cleared) })
		}
	})
	if !sub.WaitTimeout(5*time.Second) || sub.Error() != nil {
		t.Fatalf("subscribe: %v", sub.Error())
	}

	dev, errNew := New("telegram_bot", WithBroker(broker), WithClientID("dev-"+t.Name()),
		WithControls(testControls()...), WithWriteHandler(recorder.handle), WithStaleWindow(500*time.Millisecond))
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	t.Cleanup(func() { _ = dev.Stop() })

	runDone := make(chan error, 1)

	go func() { runDone <- dev.Run(context.Background()) }()

	select {
	case <-cleared:
	case <-time.After(10 * time.Second):
		t.Fatal("meta/error was not cleared")
	}

	obs.publish(t, "/devices/telegram_bot/controls/send/on", "сразу", false)

	select {
	case errRun := <-runDone:
		if errRun != nil {
			t.Fatalf("Run: %v", errRun)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}

	if !dev.Connected() {
		t.Fatal("device is not ready after Run")
	}

	waitFor(t, "write right after meta/error cleared", func() bool {
		got := recorder.writes()

		return len(got) == 1 && got[0] == "send=сразу"
	})
}

// TestDeviceShutdownExpiredContext проверяет, что Shutdown с уже истёкшим ctx всё равно
// публикует meta/error = r (за отведённый минимум времени) и не возвращает ошибку.
func TestDeviceShutdownExpiredContext(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)
	dev := startDevice(t, broker, nil, testControls()...)
	obs := newObserver(t, broker, "/devices/telegram_bot/meta/error")

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()

	<-ctx.Done()

	errShutdown := dev.Shutdown(ctx)
	if errShutdown != nil {
		t.Fatalf("Shutdown: %v", errShutdown)
	}

	waitFor(t, "error flag r", func() bool {
		value, _ := obs.get("/devices/telegram_bot/meta/error")

		return value == ErrorFlagRead
	})
}

// TestDeviceShutdownMarksError проверяет, что при остановке выставляется meta/error = r.
func TestDeviceShutdownMarksError(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)
	dev := startDevice(t, broker, nil, testControls()...)
	obs := newObserver(t, broker, "/devices/telegram_bot/meta/error")

	errShutdown := dev.Shutdown(context.Background())
	if errShutdown != nil {
		t.Fatalf("Shutdown: %v", errShutdown)
	}

	waitFor(t, "error flag r", func() bool {
		value, _ := obs.get("/devices/telegram_bot/meta/error")

		return value == ErrorFlagRead
	})

	if dev.Connected() {
		t.Fatal("device must be disconnected after Shutdown")
	}

	if errValue := dev.SetValue("send", "x"); !errors.Is(errValue, ErrNotConnected) {
		t.Fatalf("SetValue after shutdown = %v, want ErrNotConnected", errValue)
	}
}

// TestNewValidates проверяет отказ на некорректных идентификаторах и опциях.
func TestNewValidates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		deviceID string
		opts     []Option
	}{
		{name: "bad device id", deviceID: "Telegram-Bot"},
		{name: "bad control id", deviceID: "telegram_bot", opts: []Option{WithControls(Control{ID: "cmd/gate", Meta: ControlMeta{Type: ControlTypeText}})}},
		{name: "duplicate control", deviceID: "telegram_bot", opts: []Option{WithControls(testControls()[0], testControls()[0])}},
		{name: "empty broker", deviceID: "telegram_bot", opts: []Option{WithBroker("")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, errNew := New(tt.deviceID, tt.opts...)
			if !errors.Is(errNew, ErrInvalidArgument) {
				t.Fatalf("New error = %v, want ErrInvalidArgument", errNew)
			}
		})
	}
}
