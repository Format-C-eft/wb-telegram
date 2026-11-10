package wb_history

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"

	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

// waitPending ждёт регистрации запроса без зависимости от задержки брокера.
func waitPending(t *testing.T, client *Client) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		active := client.pending != nil
		client.mu.Unlock()

		if active {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal("pending not registered")
}

// TestReadCancellation проверяет отмену и освобождение единственного pending.
func TestReadCancellation(t *testing.T) {
	t.Parallel()

	client, errNew := New(WithBroker(localBroker(t)))
	if errNew != nil {
		t.Fatal(errNew)
	}

	t.Cleanup(func() { _ = client.Stop() })

	errRun := client.Run(context.Background())
	if errRun != nil {
		t.Fatal(errRun)
	}

	awaitReady(t, client)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)

	go func() { _, errRead := client.Read(ctx, historyQuery()); result <- errRead }()

	waitPending(t, client)
	cancel()

	if !errors.Is(<-result, ErrCanceled) {
		t.Fatal("cancel not propagated")
	}

	client.mu.Lock()
	active := client.pending != nil
	client.mu.Unlock()

	if active {
		t.Fatal("pending leaked")
	}

	short, shortCancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer shortCancel()

	_, errShort := client.Read(short, historyQuery())
	if !errors.Is(errShort, ErrTimeout) {
		t.Fatal(errShort)
	}
}

// TestStopPending проверяет остановку во время ожидания отсутствующей базы.
func TestStopPending(t *testing.T) {
	t.Parallel()

	client, errNew := New(WithBroker(localBroker(t)))
	if errNew != nil {
		t.Fatal(errNew)
	}

	errRun := client.Run(context.Background())
	if errRun != nil {
		t.Fatal(errRun)
	}

	awaitReady(t, client)

	result := make(chan error, 1)

	go func() { _, errRead := client.Read(context.Background(), historyQuery()); result <- errRead }()

	waitPending(t, client)

	errStop := client.Stop()
	if errStop != nil {
		t.Fatal(errStop)
	}

	if !errors.Is(<-result, ErrClosed) {
		t.Fatal("stop did not close query")
	}

	errAgain := client.Run(context.Background())
	if !errors.Is(errAgain, ErrClosed) {
		t.Fatal(errAgain)
	}
}

// TestBrokerUnavailableStart проверяет неблокирующий запуск без брокера.
func TestBrokerUnavailableStart(t *testing.T) {
	t.Parallel()

	client, errNew := New(WithBroker("tcp://127.0.0.1:1"))
	if errNew != nil {
		t.Fatal(errNew)
	}

	started := time.Now()

	errRun := client.Run(context.Background())
	if errRun != nil {
		t.Fatal(errRun)
	}

	if time.Since(started) > time.Second {
		t.Fatal("Run waited for broker")
	}

	_, errRead := client.Read(context.Background(), historyQuery())
	if !errors.Is(errRead, ErrUnavailable) {
		t.Fatal(errRead)
	}

	errStop := client.Stop()
	if errStop != nil {
		t.Fatal(errStop)
	}
}

// TestReconnectNoReplay проверяет завершение запроса при обрыве без его воспроизведения.
func TestReconnectNoReplay(t *testing.T) {
	t.Parallel()

	server, broker := localBrokerServer(t)
	counter := &requestCounter{seen: make(chan struct{}, 2)}

	errHook := server.AddHook(counter, nil)
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

	awaitReady(t, client)

	result := make(chan error, 1)

	go func() { _, errRead := client.Read(context.Background(), historyQuery()); result <- errRead }()

	waitPending(t, client)

	select {
	case <-counter.seen:
	case <-time.After(time.Second):
		t.Fatal("request missing at broker")
	}

	id := client.topic[strings.LastIndex(client.topic, "/")+1:]

	remote, ok := server.Clients.Get(id)
	if !ok {
		t.Fatal("broker client missing")
	}

	errClose := remote.Net.Conn.Close()
	if errClose != nil {
		t.Fatal(errClose)
	}

	select {
	case errRead := <-result:
		if !errors.Is(errRead, ErrUnavailable) {
			t.Fatal(errRead)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not fail query")
	}

	awaitReady(t, client)
	client.mu.Lock()
	active := client.pending != nil
	sequence := client.sequence
	client.mu.Unlock()

	if active || sequence != 1 || counter.count.Load() != 1 {
		t.Fatal("request was replayed")
	}
}

// requestCounter считает запросы истории на брокере.
type requestCounter struct {
	mqtt.HookBase
	count atomic.Int64
	seen  chan struct{}
}

// ID возвращает имя тестового хука.
func (*requestCounter) ID() string { return "history-counter" }

// Provides задаёт единственное событие хука.
func (*requestCounter) Provides(event byte) bool { return event == mqtt.OnPublish }

// OnPublish считает запросы, исключая ответы.
func (counter *requestCounter) OnPublish(_ *mqtt.Client, packet packets.Packet) (packets.Packet, error) {
	if strings.HasPrefix(packet.TopicName, "/rpc/v1/db_logger/history/get_values/") && !strings.HasSuffix(packet.TopicName, "/reply") {
		counter.count.Add(1)

		select {
		case counter.seen <- struct{}{}:
		default:
		}
	}

	return packet, nil
}
