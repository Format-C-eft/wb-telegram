package wbdevice

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/wbdevice/mock"
)

// TestWaitSubscribed проверяет SUBACK, отказ, неизвестный токен и таймаут.
func TestWaitSubscribed(t *testing.T) {
	t.Parallel()

	for _, scenario := range []string{"granted", "denied", "missing", "unexpected", "mqtt", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()

			controller := gomock.NewController(t)
			topic := controlOnTopic("telegram_bot", "send")

			token := mock.NewMockSubscriptionToken(controller)
			if scenario != "unexpected" {
				token.EXPECT().WaitTimeout(time.Millisecond).Return(scenario != "timeout")
			}

			var (
				want         error = ErrMQTT
				errSubscribe error
			)

			switch scenario {
			case "timeout":
				want = ErrTimeout
			case "mqtt":
				token.EXPECT().Error().Return(errors.New("subscribe denied"))
			case "unexpected":
				unexpected := mock.NewMockToken(controller)
				unexpected.EXPECT().WaitTimeout(time.Millisecond).Return(true)
				unexpected.EXPECT().Error().Return(nil)
				errSubscribe = waitSubscribed(unexpected, topic, time.Millisecond)
			default:
				token.EXPECT().Error().Return(nil)

				result := map[string]byte{}
				if scenario != "missing" {
					result[topic] = 0x80
				}

				if scenario == "granted" {
					result[topic] = 1
					want = nil
				}

				token.EXPECT().Result().Return(result)
			}

			if scenario != "unexpected" {
				errSubscribe = waitSubscribed(token, topic, time.Millisecond)
			}

			if !errors.Is(errSubscribe, want) {
				t.Fatalf("waitSubscribed = %v, want %v", errSubscribe, want)
			}
		})
	}
}

// TestDeniedWriteSubscription проверяет отсутствие готовности при отказе SUBACK.
func TestDeniedWriteSubscription(t *testing.T) {
	t.Parallel()

	dev, client, _, controller := newMockDevice(t)
	client.EXPECT().IsConnectionOpen().Return(true).AnyTimes()
	expectState(client, controller, "", "{}", "")
	expectCollection(client, controller, "/devices/telegram_bot/controls/#")

	topic := controlOnTopic(dev.id, "send")
	client.EXPECT().Subscribe(topic, byte(1), gomock.Any()).Return(subscribedToken(controller, topic, 0x80))

	errSetup := dev.setup(dev.nextGeneration())
	if !errors.Is(errSetup, ErrMQTT) || dev.Connected() {
		t.Fatalf("setup = %v, ready = %v", errSetup, dev.Connected())
	}
}
