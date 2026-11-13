package wbdevice

import (
	"errors"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

// deniedSubscription подменяет результат подписки на указанный топик.
type deniedSubscription struct {
	mqtt.HookBase
	topic string
}

// ID возвращает имя тестового хука.
func (*deniedSubscription) ID() string { return "denied-subscription" }

// Provides объявляет событие подтверждения подписки.
func (*deniedSubscription) Provides(event byte) bool { return event == mqtt.OnSubscribed }

// OnSubscribed возвращает отказ для указанного топика.
func (h *deniedSubscription) OnSubscribed(_ *mqtt.Client, packet packets.Packet, codes []byte) {
	for index, filter := range packet.Filters {
		if filter.Filter == h.topic {
			codes[index] = 0x80
		}
	}
}

// TestDeniedWriteSubscription проверяет готовность после отказа в подписке.
func TestDeniedWriteSubscription(t *testing.T) {
	t.Parallel()

	_, broker := startBrokerServer(t, &deniedSubscription{topic: controlOnTopic("telegram_bot", "send")})

	device, errNew := New("telegram_bot", WithBroker(broker), WithControls(testControls()...), WithTimeout(time.Second))
	if errNew != nil {
		t.Fatal(errNew)
	}

	t.Cleanup(func() { _ = device.Stop() })

	errRun := device.Run(t.Context())
	if errRun != nil {
		t.Fatal(errRun)
	}

	if device.Connected() {
		t.Fatal("device ready after subscription denial")
	}
}

// TestPurgeDeniedSubscription проверяет ошибку при отказе подписки уборки.
func TestPurgeDeniedSubscription(t *testing.T) {
	t.Parallel()

	_, broker := startBrokerServer(t, &deniedSubscription{topic: "/devices/telegram_bot/#"})

	errPurge := Purge(broker, "telegram_bot", time.Millisecond)
	if !errors.Is(errPurge, ErrMQTT) {
		t.Fatalf("Purge = %v, want MQTT error", errPurge)
	}
}

// TestLateConnectionLostPreservesReady проверяет запоздавший callback прежнего соединения.
func TestLateConnectionLostPreservesReady(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)

	device, errNew := New("telegram_bot", WithBroker(broker), WithControls(testControls()...))
	if errNew != nil {
		t.Fatal(errNew)
	}

	t.Cleanup(func() { _ = device.Stop() })

	errRun := device.Run(t.Context())
	if errRun != nil {
		t.Fatal(errRun)
	}

	device.onConnectionLost(device.client, errors.New("previous connection"))

	if !device.Connected() {
		t.Fatal("late callback invalidated the current connection")
	}
}
