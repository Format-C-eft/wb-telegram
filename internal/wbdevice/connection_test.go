package wbdevice

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/wbdevice/mock"
)

// TestDeviceRunHonorsContext проверяет отмену ожидания CONNECT и готовности.
func TestDeviceRunHonorsContext(t *testing.T) {
	t.Parallel()

	for _, connected := range []bool{false, true} {
		t.Run(map[bool]string{false: "connect", true: "setup"}[connected], func(t *testing.T) {
			t.Parallel()

			dev, client, _, controller := newMockDevice(t, WithTimeout(time.Minute))
			token := mock.NewMockToken(controller)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan struct{})
			if connected {
				close(done)
				token.EXPECT().Error().DoAndReturn(func() error {
					cancel()

					return nil
				})
			} else {
				cancel()
			}

			token.EXPECT().Done().Return(done)
			client.EXPECT().Connect().Return(token)

			errRun := dev.Run(ctx)
			if errRun != nil || dev.ready.Load() {
				t.Fatalf("Run = %v, ready = %v", errRun, dev.ready.Load())
			}
		})
	}
}

// TestDeviceRunOffline проверяет фоновое ожидание недоступного брокера.
func TestDeviceRunOffline(t *testing.T) {
	t.Parallel()

	dev, client, configured, controller := newMockDevice(t, WithTimeout(time.Millisecond))
	token := mock.NewMockToken(controller)
	token.EXPECT().Done().Return(make(chan struct{}))
	client.EXPECT().Connect().Return(token)

	errRun := dev.Run(t.Context())
	if errRun != nil || dev.ready.Load() {
		t.Fatalf("Run = %v, ready = %v", errRun, dev.ready.Load())
	}

	client.EXPECT().IsConnectionOpen().Return(true).AnyTimes()
	expectState(client, controller, "", "{}", "")
	expectCollection(client, controller, "/devices/telegram_bot/controls/#")
	expectWrites(client, controller)
	expectPublish(client, controller, deviceErrorTopic(dev.id), "")
	configured.OnConnect(client)

	if !dev.Connected() {
		t.Fatal("device did not become ready after late connection")
	}
}

// TestDeviceRunConnectError проверяет отказ подключения.
func TestDeviceRunConnectError(t *testing.T) {
	t.Parallel()

	dev, client, _, controller := newMockDevice(t)
	token := mock.NewMockToken(controller)
	done := make(chan struct{})
	close(done)
	token.EXPECT().Done().Return(done)
	token.EXPECT().Error().Return(errors.New("connect denied")).Times(2)
	client.EXPECT().Connect().Return(token)

	errRun := dev.Run(t.Context())
	if !errors.Is(errRun, ErrMQTT) {
		t.Fatalf("Run = %v", errRun)
	}
}

// TestDeviceSetupRetries проверяет повторную публикацию после отсутствия PUBACK.
func TestDeviceSetupRetries(t *testing.T) {
	t.Parallel()

	dev, client, configured, controller := newMockDevice(t)
	client.EXPECT().IsConnectionOpen().Return(true).AnyTimes()

	timeout := mock.NewMockToken(controller)
	timeout.EXPECT().WaitTimeout(gomock.Any()).Return(false)
	first := client.EXPECT().Publish(deviceMetaTopic(dev.id), byte(1), true, gomock.Any()).Return(timeout)
	retried := expectPublish(client, controller, deviceMetaTopic(dev.id), `{"driver":"wb-telegram","title":{"en":"Telegram Bot","ru":"Telegram-бот"}}`).After(first)
	expectPublish(client, controller, controlMetaTopic(dev.id, "send"), `{"type":"text","title":{"en":"Message","ru":"Сообщение"},"order":1}`).After(retried)
	expectPublish(client, controller, controlErrorTopic(dev.id, "send"), "")
	expectPublish(client, controller, controlMetaTopic(dev.id, "cmd_gate"), `{"type":"text","title":{"en":"/gate","ru":"Ворота"},"order":2,"readonly":true}`)
	expectPublish(client, controller, controlTopic(dev.id, "cmd_gate"), "{}")
	expectPublish(client, controller, controlErrorTopic(dev.id, "cmd_gate"), "")
	expectCollection(client, controller, "/devices/telegram_bot/controls/#")
	expectWrites(client, controller)
	expectPublish(client, controller, deviceErrorTopic(dev.id), "")
	configured.OnConnect(client)

	if !dev.Connected() {
		t.Fatal("setup did not recover after PUBACK timeout")
	}
}

// TestDeviceSetupStopsRetry проверяет прерывание повторов остановкой.
func TestDeviceSetupStopsRetry(t *testing.T) {
	t.Parallel()

	dev, client, configured, controller := newMockDevice(t)
	client.EXPECT().IsConnectionOpen().Return(true)

	token := mock.NewMockToken(controller)
	token.EXPECT().WaitTimeout(gomock.Any()).Return(false)
	client.EXPECT().Publish(deviceMetaTopic(dev.id), byte(1), true, gomock.Any()).DoAndReturn(func(string, byte, bool, any) any {
		dev.stopOnce.Do(func() { close(dev.stop) })

		return token
	})
	configured.OnConnect(client)

	if dev.ready.Load() {
		t.Fatal("stopped setup marked device ready")
	}
}
