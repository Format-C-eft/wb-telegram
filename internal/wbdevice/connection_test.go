package wbdevice

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// TestDeviceOnConnection проверяет события связи с брокером: готовность при подключении,
// потеря и повторная готовность после обрыва.
func TestDeviceOnConnection(t *testing.T) {
	t.Parallel()

	srv, broker := startBrokerServer(t)

	var (
		mu     sync.Mutex
		events []bool
	)

	// snapshot возвращает копию событий.
	snapshot := func() []bool {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(events)
	}

	dev, errNew := New("telegram_bot",
		WithBroker(broker),
		WithClientID("dev-"+t.Name()),
		WithStaleWindow(200*time.Millisecond),
		WithOnConnection(func(ok bool) {
			mu.Lock()

			events = append(events, ok)

			mu.Unlock()
		}),
	)
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	t.Cleanup(func() { _ = dev.Stop() })

	errRun := dev.Run(context.Background())
	if errRun != nil {
		t.Fatalf("Run: %v", errRun)
	}

	waitFor(t, "ready event", func() bool { return slices.Equal(snapshot(), []bool{true}) })

	client, ok := srv.Clients.Get("dev-" + t.Name())
	if !ok {
		t.Fatal("device client not found on broker")
	}

	client.Stop(errors.New("test: connection dropped"))

	waitFor(t, "lost and ready again", func() bool { return slices.Equal(snapshot(), []bool{true, false, true}) })
}
