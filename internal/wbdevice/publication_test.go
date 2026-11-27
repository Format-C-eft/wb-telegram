package wbdevice

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/wbdevice/mock"
)

// TestSetValueCancellationBeforePublish проверяет отмену ожидания блокировки.
func TestSetValueCancellationBeforePublish(t *testing.T) {
	t.Parallel()

	dev, _, _, _ := newMockDevice(t)

	dev.controlLock <- struct{}{}
	defer dev.unlockControls()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	errPublish := dev.SetValue(ctx, "cmd_gate", "expired")
	if !errors.Is(errPublish, context.Canceled) {
		t.Fatalf("SetValue = %v", errPublish)
	}
}

// TestSetValueUnconfirmed проверяет таймаут, отмену и остановку ожидания PUBACK.
func TestSetValueUnconfirmed(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{"timeout", "cancel", "stop"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()

			dev, client, _, controller := newMockDevice(t, WithTimeout(20*time.Millisecond))
			client.EXPECT().IsConnectionOpen().Return(true)

			token := mock.NewMockToken(controller)
			token.EXPECT().Done().Return(make(chan struct{})).AnyTimes()
			published := make(chan struct{})

			client.EXPECT().Publish(controlTopic(dev.id, "cmd_gate"), byte(1), true, "unconfirmed").DoAndReturn(func(string, byte, bool, any) any {
				close(published)

				return token
			})

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			result := make(chan error, 1)
			go func() { result <- dev.SetValue(ctx, "cmd_gate", "unconfirmed") }()

			<-published

			switch reason {
			case "cancel":
				cancel()
			case "stop":
				dev.stopOnce.Do(func() { close(dev.stop) })
			}

			select {
			case errPublish := <-result:
				if !errors.Is(errPublish, ErrTimeout) || dev.values["cmd_gate"] != "{}" {
					t.Fatalf("SetValue = %v, value = %q", errPublish, dev.values["cmd_gate"])
				}
			case <-time.After(time.Second):
				t.Fatal("SetValue did not stop waiting for PUBACK")
			}
		})
	}
}

// TestWaitPublished проверяет отказ MQTT и отсутствие PUBACK.
func TestWaitPublished(t *testing.T) {
	t.Parallel()

	for _, timedOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "mqtt", true: "timeout"}[timedOut], func(t *testing.T) {
			t.Parallel()

			controller := gomock.NewController(t)
			token := mock.NewMockToken(controller)
			token.EXPECT().WaitTimeout(time.Millisecond).Return(!timedOut)

			want := ErrTimeout

			if !timedOut {
				token.EXPECT().Error().Return(errors.New("publish denied"))

				want = ErrMQTT
			}

			errPublish := waitPublished(token, "/devices/telegram_bot/meta", time.Millisecond)
			if !errors.Is(errPublish, want) {
				t.Fatalf("waitPublished = %v, want %v", errPublish, want)
			}
		})
	}
}
