package wbdevice

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSetValueCancellationBeforePublish проверяет отмену до постановки в MQTT.
func TestSetValueCancellationBeforePublish(t *testing.T) {
	t.Parallel()

	dev, errNew := New("telegram_bot", WithControls(testControls()...))
	if errNew != nil {
		t.Fatal(errNew)
	}

	dev.controlLock <- struct{}{}
	defer dev.unlockControls()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	errPublish := dev.SetValue(ctx, "cmd_gate", `{"id":"expired"}`)
	if !errors.Is(errPublish, context.DeadlineExceeded) {
		t.Fatalf("SetValue = %v, want context deadline", errPublish)
	}
}

// TestSetValueCancellationAfterPublish проверяет отмену ожидания PUBACK.
func TestSetValueCancellationAfterPublish(t *testing.T) {
	t.Parallel()

	srv, broker := startBrokerServer(t)
	dev := startDevice(t, broker, nil, testControls()...)

	hook := &rejectOnceHook{topic: controlTopic("telegram_bot", "cmd_gate")}

	errHook := srv.AddHook(hook, nil)
	if errHook != nil {
		t.Fatal(errHook)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	result := make(chan error, 1)

	go func() { result <- dev.SetValue(ctx, "cmd_gate", `{"id":"unconfirmed"}`) }()

	waitFor(t, "publication received without PUBACK", hook.rejected.Load)
	cancel()

	select {
	case errPublish := <-result:
		if !errors.Is(errPublish, ErrTimeout) {
			t.Fatalf("SetValue = %v, want unconfirmed publication", errPublish)
		}
	case <-time.After(time.Second):
		t.Fatal("SetValue waited for PUBACK after cancellation")
	}
}
