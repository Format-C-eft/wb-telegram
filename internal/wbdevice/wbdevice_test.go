package wbdevice

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/wbdevice/mock"
)

// TestDevicePublishesInitialState проверяет WB metadata, placeholder и настройки MQTT.
func TestDevicePublishesInitialState(t *testing.T) {
	t.Parallel()

	dev, client, configured, controller := newMockDevice(t)
	if configured.WillTopic != deviceErrorTopic(dev.id) || string(configured.WillPayload) != ErrorFlagRead || configured.WillQos != 1 || !configured.WillRetained || !configured.AutoReconnect || !configured.ConnectRetry || !configured.CleanSession {
		t.Fatalf("MQTT options = %+v", configured)
	}

	client.EXPECT().IsConnectionOpen().Return(true).AnyTimes()
	expectState(client, controller, "", "{}", "")
	expectCollection(client, controller, "/devices/telegram_bot/controls/#")
	expectWrites(client, controller)
	expectPublish(client, controller, deviceErrorTopic(dev.id), "")
	configured.OnConnect(client)

	if !dev.Connected() {
		t.Fatal("device is not ready after metadata and subscriptions")
	}

	runDevice(t, dev, client, controller)
}

// TestDeviceReconnect проверяет повторное состояние и сброс временного значения.
func TestDeviceReconnect(t *testing.T) {
	t.Parallel()

	var events []bool

	dev, client, configured, controller := newMockDevice(t, WithOnConnection(func(ok bool) { events = append(events, ok) }))
	client.EXPECT().IsConnectionOpen().Return(true).AnyTimes()
	expectState(client, controller, "", "{}", "")
	expectCollection(client, controller, "/devices/telegram_bot/controls/#")
	expectWrites(client, controller)
	expectPublish(client, controller, deviceErrorTopic(dev.id), "")
	configured.OnConnect(client)

	expectPublish(client, controller, controlTopic(dev.id, "send"), "saved")
	expectPublish(client, controller, controlTopic(dev.id, "cmd_gate"), `{"id":"7"}`)
	expectPublish(client, controller, controlErrorTopic(dev.id, "send"), ErrorFlagWrite)

	if errValue := dev.SetValue(t.Context(), "send", "saved"); errValue != nil {
		t.Fatal(errValue)
	}

	if errCommand := dev.SetValue(t.Context(), "cmd_gate", `{"id":"7"}`); errCommand != nil {
		t.Fatal(errCommand)
	}

	if errFlags := dev.SetError(t.Context(), "send", ErrorFlagWrite); errFlags != nil {
		t.Fatal(errFlags)
	}

	configured.OnReconnecting(client, configured)

	if dev.Connected() {
		t.Fatal("device stayed ready while reconnecting")
	}

	expectState(client, controller, "saved", "{}", ErrorFlagWrite)
	expectWrites(client, controller)
	expectPublish(client, controller, deviceErrorTopic(dev.id), "")
	configured.OnConnect(client)
	configured.OnConnectionLost(client, errors.New("previous connection"))

	if !dev.Connected() || !slices.Equal(events, []bool{true, false, true}) {
		t.Fatalf("ready = %v, events = %v", dev.Connected(), events)
	}
}

// TestDeviceSetValueFailureNotReplayed проверяет откат недоставленного значения.
func TestDeviceSetValueFailureNotReplayed(t *testing.T) {
	t.Parallel()

	dev, client, _, controller := newMockDevice(t)
	client.EXPECT().IsConnectionOpen().Return(false)

	errValue := dev.SetValue(t.Context(), "cmd_gate", "failed")
	if !errors.Is(errValue, ErrNotConnected) {
		t.Fatalf("SetValue = %v", errValue)
	}

	client.EXPECT().IsConnectionOpen().Return(true).AnyTimes()
	expectState(client, controller, "", "{}", "")

	if errState := dev.publishState(); errState != nil {
		t.Fatal(errState)
	}

	if errUnknown := dev.SetError(t.Context(), "missing", "w"); !errors.Is(errUnknown, ErrUnknownControl) {
		t.Fatalf("SetError = %v", errUnknown)
	}

	if errUnknownValue := dev.SetValue(t.Context(), "missing", "x"); !errors.Is(errUnknownValue, ErrUnknownControl) {
		t.Fatalf("SetValue = %v", errUnknownValue)
	}
}

// TestDeviceWriteHandler проверяет фильтрацию, копирование payload и очередь записей.
func TestDeviceWriteHandler(t *testing.T) {
	t.Parallel()

	delivered := make(chan string, 1)
	dev, client, _, controller := newMockDevice(t, WithWriteHandler(func(id string, payload []byte) { delivered <- id + "=" + string(payload) }))
	topic := controlOnTopic(dev.id, "send")

	var handler paho.MessageHandler

	client.EXPECT().Subscribe(topic, byte(1), gomock.Any()).Do(func(_ string, _ byte, callback paho.MessageHandler) {
		handler = callback
	}).Return(subscribedToken(controller, topic, 1))

	if errSubscribe := dev.subscribeWrites(); errSubscribe != nil {
		t.Fatal(errSubscribe)
	}

	handler(client, mockMessage(controller, topic, []byte("old"), true))
	handler(client, mockMessage(controller, "/devices/other/controls/send/on", []byte("foreign"), false))

	payload := []byte("fresh")
	handler(client, mockMessage(controller, topic, payload, false))
	payload[0] = 'X'

	dev.workers.Add(1)
	go dev.dispatchWrites()

	select {
	case got := <-delivered:
		if got != "send=fresh" {
			t.Fatalf("write = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("write not delivered")
	}

	dev.stopOnce.Do(func() { close(dev.stop) })
	dev.workers.Wait()
	handler(client, mockMessage(controller, topic, []byte("stopped"), false))

	if len(dev.writes) != 0 {
		t.Fatal("write enqueued after stop")
	}
}

// TestDeviceWriteQueueFull проверяет неблокирующий приём переполненной очереди.
func TestDeviceWriteQueueFull(t *testing.T) {
	t.Parallel()

	dev, client, _, controller := newMockDevice(t, WithWriteHandler(func(string, []byte) {}))
	for range writeQueueSize + 1 {
		dev.handleWrite(client, mockMessage(controller, controlOnTopic(dev.id, "send"), []byte("x"), false))
	}

	if len(dev.writes) != writeQueueSize {
		t.Fatalf("queue size = %d", len(dev.writes))
	}
}

// TestDeviceHandlerCanSetValue проверяет публикацию из обработчика очереди.
func TestDeviceHandlerCanSetValue(t *testing.T) {
	t.Parallel()

	var dev *Device

	result := make(chan error, 1)
	handler := func(id string, payload []byte) { result <- dev.SetValue(context.Background(), id, string(payload)) }

	var (
		client     *mock.MockClient
		controller *gomock.Controller
	)

	dev, client, _, controller = newMockDevice(t, WithWriteHandler(handler))
	client.EXPECT().IsConnectionOpen().Return(true)
	expectPublish(client, controller, controlTopic(dev.id, "send"), "reply")

	dev.workers.Add(1)
	go dev.dispatchWrites()

	dev.handleWrite(client, mockMessage(controller, controlOnTopic(dev.id, "send"), []byte("reply"), false))

	select {
	case errHandler := <-result:
		if errHandler != nil {
			t.Fatal(errHandler)
		}
	case <-time.After(time.Second):
		t.Fatal("handler blocked")
	}
}

// TestNewValidates проверяет идентификаторы и обязательные опции.
func TestNewValidates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		deviceID string
		opts     []Option
	}{
		{name: "device", deviceID: "Telegram-Bot"},
		{name: "control", deviceID: "telegram_bot", opts: []Option{WithControls(Control{ID: "cmd/gate"})}},
		{name: "duplicate", deviceID: "telegram_bot", opts: []Option{WithControls(testControls()[0], testControls()[0])}},
		{name: "broker", deviceID: "telegram_bot", opts: []Option{WithBroker("")}},
		{name: "client", deviceID: "telegram_bot", opts: []Option{WithClientID("")}},
		{name: "driver", deviceID: "telegram_bot", opts: []Option{WithDriver("")}},
		{name: "handler", deviceID: "telegram_bot", opts: []Option{WithOnConnection(nil)}},
		{name: "timeout", deviceID: "telegram_bot", opts: []Option{WithTimeout(0)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, errNew := New(tt.deviceID, tt.opts...)
			if !errors.Is(errNew, ErrInvalidArgument) {
				t.Fatalf("New = %v", errNew)
			}
		})
	}
}

// TestDeviceSubscribesBeforeHealthy проверяет порядок готовности устройства.
func TestDeviceSubscribesBeforeHealthy(t *testing.T) {
	t.Parallel()

	dev, client, configured, controller := newMockDevice(t)
	client.EXPECT().IsConnectionOpen().Return(true).AnyTimes()
	expectState(client, controller, "", "{}", "")

	filter := "/devices/telegram_bot/controls/#"
	subscribe := client.EXPECT().Subscribe(filter, byte(1), gomock.Any()).Return(subscribedToken(controller, filter, 1))
	unsubscribe := client.EXPECT().Unsubscribe(filter).Return(completedToken(controller)).After(subscribe)
	topic := controlOnTopic(dev.id, "send")
	writes := client.EXPECT().Subscribe(topic, byte(1), gomock.Any()).Return(subscribedToken(controller, topic, 1)).After(unsubscribe)
	healthy := expectPublish(client, controller, deviceErrorTopic(dev.id), "").After(writes)
	healthy.Do(func(string, byte, bool, any) {
		if dev.Connected() {
			t.Error("device ready before healthy PUBACK")
		}
	})
	configured.OnConnect(client)

	if !maps.Equal(dev.values, map[string]string{"send": "", "cmd_gate": "{}"}) {
		t.Fatalf("values = %v", dev.values)
	}
}

// TestClientFactoryIsLocal проверяет изоляцию фабрик отдельных устройств.
func TestClientFactoryIsLocal(t *testing.T) {
	t.Parallel()

	first, firstClient, _, _ := newMockDevice(t)

	second, secondClient, _, _ := newMockDevice(t)
	if first.client != firstClient || second.client != secondClient || first.client == second.client {
		t.Fatal("device clients are shared")
	}

	var _ paho.Client = firstClient
}

// TestDeviceWriteOrder проверяет последовательную доставку очереди обработчику.
func TestDeviceWriteOrder(t *testing.T) {
	t.Parallel()

	const count = 10

	delivered := make(chan string, count)

	dev, client, _, controller := newMockDevice(t, WithWriteHandler(func(_ string, payload []byte) {
		delivered <- string(payload)
	}))
	for index := range count {
		dev.handleWrite(client, mockMessage(controller, controlOnTopic(dev.id, "send"), []byte(strconv.Itoa(index)), false))
	}

	dev.workers.Add(1)
	go dev.dispatchWrites()

	for index := range count {
		select {
		case got := <-delivered:
			if got != strconv.Itoa(index) {
				t.Fatalf("write %d = %q", index, got)
			}
		case <-time.After(time.Second):
			t.Fatal("queue delivery stopped")
		}
	}
}
