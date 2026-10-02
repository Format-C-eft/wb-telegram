package wbdevice

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// seedFilter — фильтр подписки наблюдателя, который только публикует: на него никто не пишет.
const seedFilter = "/test/seed"

// TestDeviceRemovesStaleControls проверяет удаление контролов удалённых команд при подключении
// и то, что контролы текущего набора и чужие устройства остаются на месте.
func TestDeviceRemovesStaleControls(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)
	seed := newObserver(t, broker, seedFilter)

	seed.publish(t, "/devices/telegram_bot/controls/cmd_old/meta", `{"type":"text"}`, true)
	seed.publish(t, "/devices/telegram_bot/controls/cmd_old", `{"id":"1"}`, true)
	seed.publish(t, "/devices/other/controls/cmd_old/meta", `{"type":"text"}`, true)

	watch := newObserver(t, broker, "/devices/#")

	waitForRetained(t, watch, "/devices/telegram_bot/controls/cmd_old/meta")
	waitForRetained(t, watch, "/devices/telegram_bot/controls/cmd_old")
	waitForRetained(t, watch, "/devices/other/controls/cmd_old/meta")

	startDevice(t, broker, nil, testControls()...)

	waitFor(t, "stale control cleared", func() bool {
		meta, _ := watch.get("/devices/telegram_bot/controls/cmd_old/meta")
		value, _ := watch.get("/devices/telegram_bot/controls/cmd_old")

		return meta == "" && value == ""
	})

	late := newObserver(t, broker, "/devices/#")

	waitForRetained(t, late, "/devices/telegram_bot/controls/cmd_gate/meta")
	waitForRetained(t, late, "/devices/telegram_bot/controls/send/meta")
	waitForRetained(t, late, "/devices/other/controls/cmd_old/meta")

	time.Sleep(300 * time.Millisecond)

	for _, topic := range []string{"/devices/telegram_bot/controls/cmd_old/meta", "/devices/telegram_bot/controls/cmd_old"} {
		if _, ok := late.get(topic); ok {
			t.Fatalf("stale topic %s is still retained", topic)
		}
	}
}

// TestPurge проверяет полное удаление устройства из брокера без последствий для других устройств.
func TestPurge(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)
	seed := newObserver(t, broker, seedFilter)

	seed.publish(t, "/devices/telegram_bot/meta", `{"driver":"wb-telegram"}`, true)
	seed.publish(t, "/devices/telegram_bot/controls/send/meta", `{"type":"text"}`, true)
	seed.publish(t, "/devices/telegram_bot_extra/meta", `{"driver":"x"}`, true)
	seed.publish(t, "/devices/other/meta", `{"driver":"x"}`, true)

	watch := newObserver(t, broker, "/devices/#")

	waitForRetained(t, watch, "/devices/telegram_bot/meta")
	waitForRetained(t, watch, "/devices/telegram_bot/controls/send/meta")

	errPurge := Purge(broker, "telegram_bot", 200*time.Millisecond)
	if errPurge != nil {
		t.Fatalf("Purge: %v", errPurge)
	}

	late := newObserver(t, broker, "/devices/#")

	waitForRetained(t, late, "/devices/other/meta")
	waitForRetained(t, late, "/devices/telegram_bot_extra/meta")

	time.Sleep(300 * time.Millisecond)

	for _, topic := range []string{"/devices/telegram_bot/meta", "/devices/telegram_bot/controls/send/meta"} {
		if _, ok := late.get(topic); ok {
			t.Fatalf("topic %s is still retained", topic)
		}
	}
}

// TestPurgeClientIDUnique проверяет, что у каждой уборки свой client id (одинаковый id заставил бы
// брокер отключить первую уборку при подключении второй) и что id укладывается в 23 символа MQTT 3.1.1.
func TestPurgeClientIDUnique(t *testing.T) {
	t.Parallel()

	first := purgeClientID()
	second := purgeClientID()

	if first == second {
		t.Fatalf("client ids are equal: %q", first)
	}

	for _, id := range []string{first, second} {
		if !strings.HasPrefix(id, "tgpurge-") || len(id) > 23 {
			t.Fatalf("client id = %q, want prefix tgpurge- and at most 23 chars", id)
		}
	}
}

// TestPurgeErrors проверяет ошибки Purge при некорректном устройстве и недоступном брокере.
func TestPurgeErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		broker   string
		deviceID string
		want     error
	}{
		{name: "bad device id", broker: "tcp://127.0.0.1:1", deviceID: "telegram/#", want: ErrInvalidArgument},
		{name: "unreachable broker", broker: "tcp://127.0.0.1:1", deviceID: "telegram_bot", want: ErrMQTT},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			errPurge := Purge(tt.broker, tt.deviceID, 100*time.Millisecond)
			if !errors.Is(errPurge, tt.want) {
				t.Fatalf("Purge error = %v, want %v", errPurge, tt.want)
			}
		})
	}
}

// TestCollectRetainedStops проверяет, что сбор retained-сообщений прерывается остановкой устройства,
// не дожидаясь конца окна, и ничего не возвращает для удаления.
func TestCollectRetainedStops(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)
	seed := newObserver(t, broker, seedFilter)

	seed.publish(t, "/devices/telegram_bot/controls/cmd_old/meta", `{"type":"text"}`, true)

	stop := make(chan struct{})
	close(stop)

	started := time.Now()

	topics, errCollect := collectRetained(seed.client, "/devices/telegram_bot/controls/#", time.Minute, stop)
	if errCollect != nil {
		t.Fatalf("collectRetained: %v", errCollect)
	}

	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("collectRetained took %v after stop", elapsed)
	}

	if len(topics) != 0 {
		t.Fatalf("topics = %v, want none after stop", topics)
	}
}
