package wbdevice

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/wbdevice/mock"
)

// TestDeviceRemovesStaleControls проверяет удаление только устаревших retained-контролов.
func TestDeviceRemovesStaleControls(t *testing.T) {
	t.Parallel()

	dev, client, _, controller := newMockDevice(t)
	filter := "/devices/telegram_bot/controls/#"
	client.EXPECT().Subscribe(filter, byte(1), gomock.Any()).DoAndReturn(func(_ string, _ byte, handler paho.MessageHandler) paho.Token {
		for _, topic := range []string{
			"/devices/telegram_bot/controls/cmd_old/meta", "/devices/telegram_bot/controls/cmd_old",
			"/devices/telegram_bot/controls/cmd_gate/meta", "/devices/telegram_bot/controls/send/meta",
			"/devices/other/controls/cmd_old/meta", "/devices/telegram_bot_extra/controls/cmd_old/meta",
		} {
			handler(client, mockMessage(controller, topic, []byte("value"), true))
		}

		return subscribedToken(controller, filter, 1)
	})
	client.EXPECT().Unsubscribe(filter).Return(completedToken(controller))
	expectPublish(client, controller, "/devices/telegram_bot/controls/cmd_old", "")
	expectPublish(client, controller, "/devices/telegram_bot/controls/cmd_old/meta", "")
	dev.removeStale()
}

// TestPurge проверяет фильтр, порядок отключения и удаление найденных retained-топиков.
func TestPurge(t *testing.T) {
	t.Parallel()

	controller := gomock.NewController(t)
	client := mock.NewMockClient(controller)
	connect := client.EXPECT().Connect().Return(completedToken(controller))
	filter := "/devices/telegram_bot/#"
	subscribe := client.EXPECT().Subscribe(filter, byte(1), gomock.Any()).DoAndReturn(func(_ string, _ byte, handler paho.MessageHandler) paho.Token {
		handler(client, mockMessage(controller, "/devices/telegram_bot/meta", []byte("metadata"), true))
		handler(client, mockMessage(controller, "/devices/telegram_bot/controls/send/meta", []byte("control"), true))
		handler(client, mockMessage(controller, "/devices/telegram_bot/meta", []byte("metadata"), true))
		handler(client, mockMessage(controller, "/devices/telegram_bot/controls/send/on", []byte("fresh"), false))
		handler(client, mockMessage(controller, "/devices/telegram_bot/controls/empty", nil, true))

		return subscribedToken(controller, filter, 1)
	}).After(connect)
	unsubscribe := client.EXPECT().Unsubscribe(filter).Return(completedToken(controller)).After(subscribe)
	first := expectPublish(client, controller, "/devices/telegram_bot/controls/send/meta", "").After(unsubscribe)
	last := expectPublish(client, controller, "/devices/telegram_bot/meta", "").After(first)
	client.EXPECT().Disconnect(uint(250)).After(last)

	factory := func(configured *paho.ClientOptions) paho.Client {
		if configured.ClientID == "" || !configured.CleanSession || configured.ConnectTimeout != purgeTimeout || len(configured.Servers) != 1 || configured.Servers[0].String() != "tcp://localhost:1883" {
			t.Errorf("purge options = %+v", configured)
		}

		return client
	}

	errPurge := purge("tcp://localhost:1883", "telegram_bot", time.Nanosecond, factory)
	if errPurge != nil {
		t.Fatal(errPurge)
	}
}

// TestPurgeErrors проверяет ошибки валидации, подключения и подписки.
func TestPurgeErrors(t *testing.T) {
	t.Parallel()

	for _, scenario := range []string{"invalid", "timeout", "connect", "subscription"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()

			controller := gomock.NewController(t)
			client := mock.NewMockClient(controller)
			deviceID := "telegram_bot"
			want := ErrMQTT

			if scenario == "invalid" {
				deviceID = "telegram/#"
				want = ErrInvalidArgument
			} else {
				token := mock.NewMockToken(controller)
				token.EXPECT().WaitTimeout(purgeTimeout).Return(scenario != "timeout")
				client.EXPECT().Connect().Return(token)

				switch scenario {
				case "timeout":
					want = ErrTimeout
				case "connect":
					token.EXPECT().Error().Return(errors.New("connect denied")).Times(2)
				case "subscription":
					token.EXPECT().Error().Return(nil)

					filter := "/devices/telegram_bot/#"
					client.EXPECT().Subscribe(filter, byte(1), gomock.Any()).Return(subscribedToken(controller, filter, 0x80))
					client.EXPECT().Disconnect(uint(250))
				}
			}

			factory := func(*paho.ClientOptions) paho.Client {
				if scenario == "invalid" {
					t.Error("factory called before validation")
				}

				return client
			}

			errPurge := purge("tcp://localhost:1883", deviceID, time.Nanosecond, factory)
			if !errors.Is(errPurge, want) {
				t.Fatalf("purge = %v, want %v", errPurge, want)
			}
		})
	}
}

// TestCollectRetainedStops проверяет отмену сбора и обязательную отписку.
func TestCollectRetainedStops(t *testing.T) {
	t.Parallel()

	controller := gomock.NewController(t)
	client := mock.NewMockClient(controller)
	filter := "/devices/telegram_bot/controls/#"
	client.EXPECT().Subscribe(filter, byte(1), gomock.Any()).DoAndReturn(func(_ string, _ byte, handler paho.MessageHandler) paho.Token {
		handler(client, mockMessage(controller, filter, []byte("value"), true))

		return subscribedToken(controller, filter, 1)
	})
	client.EXPECT().Unsubscribe(filter).Return(completedToken(controller))

	stop := make(chan struct{})
	close(stop)

	topics, errCollect := collectRetained(client, filter, time.Hour, stop)
	if errCollect != nil || len(topics) != 0 {
		t.Fatalf("collectRetained = %v, %v", topics, errCollect)
	}
}

// TestCollectRetainedSorted проверяет сортировку и устранение повторных retained-топиков.
func TestCollectRetainedSorted(t *testing.T) {
	t.Parallel()

	controller := gomock.NewController(t)
	client := mock.NewMockClient(controller)
	filter := "/devices/telegram_bot/#"
	client.EXPECT().Subscribe(filter, byte(1), gomock.Any()).DoAndReturn(func(_ string, _ byte, handler paho.MessageHandler) paho.Token {
		for _, topic := range []string{"z", "a", "z"} {
			handler(client, mockMessage(controller, topic, []byte("value"), true))
		}

		return subscribedToken(controller, filter, 1)
	})
	client.EXPECT().Unsubscribe(filter).Return(completedToken(controller))

	topics, errCollect := collectRetained(client, filter, time.Nanosecond, nil)
	if errCollect != nil || !slices.Equal(topics, []string{"a", "z"}) {
		t.Fatalf("collectRetained = %v, %v", topics, errCollect)
	}
}

// TestClearTopicsErrors проверяет прекращение уборки при ошибке публикации.
func TestClearTopicsErrors(t *testing.T) {
	t.Parallel()

	for _, timedOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "mqtt", true: "timeout"}[timedOut], func(t *testing.T) {
			t.Parallel()

			controller := gomock.NewController(t)
			client := mock.NewMockClient(controller)
			token := mock.NewMockToken(controller)
			client.EXPECT().Publish("first", byte(1), true, "").Return(token)
			token.EXPECT().WaitTimeout(purgeTimeout).Return(!timedOut)

			want := ErrTimeout

			if !timedOut {
				token.EXPECT().Error().Return(errors.New("publish denied")).Times(2)

				want = ErrMQTT
			}

			errClear := clearTopics(client, []string{"first", "second"})
			if !errors.Is(errClear, want) {
				t.Fatalf("clearTopics = %v", errClear)
			}
		})
	}
}

// TestPurgeClientIDUnique проверяет уникальность и допустимую длину идентификатора.
func TestPurgeClientIDUnique(t *testing.T) {
	t.Parallel()

	first := purgeClientID()

	second := purgeClientID()
	if first == second {
		t.Fatalf("client ids are equal: %q", first)
	}

	for _, id := range []string{first, second} {
		if !strings.HasPrefix(id, "tgpurge-") || len(id) > 23 {
			t.Fatalf("invalid client id = %q", id)
		}
	}
}
