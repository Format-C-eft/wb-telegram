package wb_history

import (
	"context"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/wb_history/mock"
)

// TestBrokerAddressAliases проверяет передачу адресов MQTT без изменения схемы.
func TestBrokerAddressAliases(t *testing.T) {
	t.Parallel()

	for _, broker := range []string{"127.0.0.1:1883", "mqtt://127.0.0.1:1883"} {
		t.Run(broker, func(t *testing.T) {
			t.Parallel()

			fixture := newHistoryFixture(t, WithBroker(broker))
			if len(fixture.config.Servers) != 1 || fixture.config.Servers[0].Host != "127.0.0.1:1883" {
				t.Fatalf("servers=%v", fixture.config.Servers)
			}

			fixture.run(t)
		})
	}
}

// retryClient создаёт клиент с отказом первого SUBACK.
func retryClient(t *testing.T, retry bool) (*historyFixture, <-chan struct{}) {
	t.Helper()

	fixture := newHistoryFixture(t)
	fixture.mqtt.EXPECT().Connect().Return(completedToken(fixture.ctrl, nil))
	fixture.mqtt.EXPECT().IsConnectionOpen().Return(true).AnyTimes()

	first := make(chan struct{})
	failed := mock.NewMockSubscriptionToken(fixture.ctrl)
	ack := make(chan struct{})
	close(ack)
	failed.EXPECT().Done().Return(ack)
	failed.EXPECT().Error().Return(nil)
	failed.EXPECT().Result().DoAndReturn(func() map[string]byte {
		close(first)

		return map[string]byte{fixture.client.topic + "/reply": 0x80}
	})
	fixture.mqtt.EXPECT().Subscribe(fixture.client.topic+"/reply", byte(1), gomock.Any()).DoAndReturn(func(_ string, _ byte, handler paho.MessageHandler) paho.Token {
		fixture.handler = handler

		return failed
	})

	if retry {
		fixture.expectSubscribe(fixture.subscription(1))
	}

	errRun := fixture.client.Run(context.Background())
	if errRun != nil {
		t.Fatal(errRun)
	}

	done := make(chan struct{})

	go func() { fixture.config.OnConnect(fixture.mqtt); close(done) }()

	<-first

	return fixture, done
}

// TestSubscribeRetry проверяет повтор подписки после отказа без переподключения.
func TestSubscribeRetry(t *testing.T) {
	t.Parallel()

	fixture, done := retryClient(t, true)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SUBACK retry did not finish")
	}

	if !fixture.client.Ready() {
		t.Fatal("retry did not restore ready")
	}
}

// TestSubscribeRetryStop проверяет остановку в паузе повтора подписки.
func TestSubscribeRetryStop(t *testing.T) {
	t.Parallel()

	fixture, done := retryClient(t, false)
	started := time.Now()

	errStop := fixture.client.Stop()
	if errStop != nil {
		t.Fatal(errStop)
	}

	<-done

	if time.Since(started) > time.Second || fixture.client.Ready() {
		t.Fatal("Stop waited for retry or kept ready")
	}
}

// TestSubscribeRetryGeneration проверяет завершение устаревшего setup.
func TestSubscribeRetryGeneration(t *testing.T) {
	t.Parallel()

	fixture, done := retryClient(t, false)
	fixture.config.OnReconnecting(fixture.mqtt, fixture.config)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stale setup did not exit")
	}

	if fixture.client.Ready() {
		t.Fatal("stale generation subscribed")
	}
}
