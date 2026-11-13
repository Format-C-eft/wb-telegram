package wb_history

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

// TestBrokerAddressAliases проверяет адреса MQTT с tcp и без явной схемы.
func TestBrokerAddressAliases(t *testing.T) {
	t.Parallel()

	_, broker := localBrokerServer(t)
	address := strings.TrimPrefix(broker, "tcp://")

	for _, prefix := range []string{"", "mqtt://"} {
		t.Run(prefix+address, func(t *testing.T) {
			t.Parallel()

			client, errNew := New(WithBroker(prefix + address))
			if errNew != nil {
				t.Fatal(errNew)
			}

			t.Cleanup(func() { _ = client.Stop() })

			errRun := client.Run(t.Context())
			if errRun != nil {
				t.Fatal(errRun)
			}

			awaitReady(t, client)
		})
	}
}

// subscribeFault отказывает в первом SUBACK без потери соединения.
type subscribeFault struct {
	mqtt.HookBase
	attempts atomic.Int64
	connects atomic.Int64
	always   bool
	first    chan struct{}
}

// ID возвращает имя тестового хука.
func (*subscribeFault) ID() string { return "subscribe-fault" }

// Provides задаёт события тестового хука.
func (*subscribeFault) Provides(event byte) bool {
	return event == mqtt.OnConnect || event == mqtt.OnSubscribed
}

// OnConnect считает подключения клиента.
func (fault *subscribeFault) OnConnect(_ *mqtt.Client, _ packets.Packet) error {
	fault.connects.Add(1)

	return nil
}

// OnSubscribed заменяет первый успешный SUBACK явным отказом.
func (fault *subscribeFault) OnSubscribed(_ *mqtt.Client, _ packets.Packet, codes []byte) {
	attempt := fault.attempts.Add(1)
	if attempt == 1 || fault.always {
		codes[0] = 0x80
	}

	if attempt == 1 {
		close(fault.first)
	}
}

// retryClient создаёт клиент с ошибкой первого подтверждения подписки.
func retryClient(t *testing.T, always bool) (*Client, *subscribeFault) {
	t.Helper()

	server, broker := localBrokerServer(t)
	fault := &subscribeFault{always: always, first: make(chan struct{})}

	errHook := server.AddHook(fault, nil)
	if errHook != nil {
		t.Fatal(errHook)
	}

	client, errNew := New(WithBroker(broker))
	if errNew != nil {
		t.Fatal(errNew)
	}

	t.Cleanup(func() { _ = client.Stop() })

	errRun := client.Run(context.Background())
	if errRun != nil {
		t.Fatal(errRun)
	}

	select {
	case <-fault.first:
	case <-time.After(2 * time.Second):
		t.Fatal("first SUBACK missing")
	}

	return client, fault
}

// TestSubscribeRetry проверяет повтор подписки после отказа без переподключения.
func TestSubscribeRetry(t *testing.T) {
	t.Parallel()

	client, fault := retryClient(t, false)
	awaitReady(t, client)

	if fault.connects.Load() != 1 || fault.attempts.Load() != 2 {
		t.Fatalf("connects=%d subscriptions=%d", fault.connects.Load(), fault.attempts.Load())
	}
}

// TestSubscribeRetryStop проверяет завершение setup при остановке в паузе повтора.
func TestSubscribeRetryStop(t *testing.T) {
	t.Parallel()

	client, fault := retryClient(t, true)
	started := time.Now()

	errStop := client.Stop()
	if errStop != nil {
		t.Fatal(errStop)
	}

	if time.Since(started) > time.Second {
		t.Fatal("Stop waited for retry timeout")
	}

	if client.Ready() || fault.attempts.Load() != 1 || fault.connects.Load() != 1 {
		t.Fatal("subscription retried after Stop")
	}
}

// TestSubscribeRetryGeneration проверяет завершение повтора устаревшего соединения.
func TestSubscribeRetryGeneration(t *testing.T) {
	t.Parallel()

	client, fault := retryClient(t, true)
	client.onReconnecting(nil, nil)

	done := make(chan struct{})

	go func() { client.setups.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stale setup did not exit")
	}

	if client.Ready() || fault.attempts.Load() != 1 {
		t.Fatal("stale generation subscribed")
	}
}
