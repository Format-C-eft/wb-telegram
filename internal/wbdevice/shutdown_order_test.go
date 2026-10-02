package wbdevice

import (
	"context"
	"testing"
	"time"
)

// TestDeviceStaleSetupKeepsShutdownFlag проверяет, что пустой флаг ошибки от устаревшего соединения
// не перезаписывает "r", выставленный при остановке.
func TestDeviceStaleSetupKeepsShutdownFlag(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)
	dev := startDevice(t, broker, nil)
	obs := newObserver(t, broker, "/devices/telegram_bot/meta/error")
	topic := deviceErrorTopic("telegram_bot")

	dev.stateMu.Lock()
	stale := dev.generation
	dev.stateMu.Unlock()

	// Как в Shutdown: смена поколения и "r" под flagMu.
	dev.flagMu.Lock()
	dev.nextGeneration()

	errPublish := dev.publish(topic, ErrorFlagRead)

	dev.flagMu.Unlock()

	if errPublish != nil {
		t.Fatalf("publish r: %v", errPublish)
	}

	waitFor(t, "device error r", func() bool {
		value, _ := obs.get(topic)

		return value == ErrorFlagRead
	})

	errStale := dev.publishHealthy(stale)
	if errStale != nil {
		t.Fatalf("publishHealthy(stale): %v", errStale)
	}

	time.Sleep(300 * time.Millisecond)

	if value, _ := obs.get(topic); value != ErrorFlagRead {
		t.Fatalf("meta/error = %q after stale setup, want %q", value, ErrorFlagRead)
	}
}

// TestDeviceRunHonorsContext проверяет, что отмена контекста прерывает ожидание готовности в Run.
func TestDeviceRunHonorsContext(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)

	dev, errNew := New("telegram_bot",
		WithBroker(broker),
		WithClientID("dev-"+t.Name()),
		WithStaleWindow(5*time.Second),
	)
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	t.Cleanup(func() { _ = dev.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()

	errRun := dev.Run(ctx)
	if errRun != nil {
		t.Fatalf("Run: %v", errRun)
	}

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Run returned after %v, want soon after context cancel", elapsed)
	}
}
