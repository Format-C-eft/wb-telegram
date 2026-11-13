package bot

import (
	"context"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// TestEphemeralReplyLifetime проверяет источник, срок и однократность ответа.
func TestEphemeralReplyLifetime(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		callbackID string
		age        time.Duration
		wantSend   bool
	}{
		{name: "button", callbackID: "press", wantSend: true},
		{name: "manual command"},
		{name: "expired", callbackID: "press", age: shortReplyWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, deps := newTestBot(t)
			t.Cleanup(func() { _ = b.Stop() })

			sent := make(chan struct{})

			created := testNow.Add(-tc.age)
			if !b.track("50", 100, "gate", created, commandSource{callbackID: tc.callbackID}) {
				t.Fatal("command was not tracked")
			}

			if tc.wantSend {
				deps.sender.EXPECT().SendEphemeral(gomock.Any(), "press", telegram.Delivery{Kind: telegram.DeliveryText, Text: "22 °C"}).
					DoAndReturn(func(context.Context, string, telegram.Delivery) error {
						close(sent)

						return nil
					})
			}

			reply := []byte(`{"text":"22 °C","ephemeral":true,"reply_to":"50"}`)
			b.HandleSend(reply)
			b.HandleSend(reply)

			if tc.wantSend {
				awaitSignal(t, sent)
			}
		})
	}
}

// TestCallbackReplyTimer проверяет срок ожидания ответа на нажатие.
func TestCallbackReplyTimer(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	t.Cleanup(func() { _ = b.Stop() })

	b.cfg.ReplyTimeoutS = 1

	if !b.track("50", 100, "gate", testNow, commandSource{callbackID: "press"}) {
		t.Fatal("command was not tracked")
	}

	b.startReplyTimer("50")

	if got := deps.timers.duration(0); got != shortReplyWindow {
		t.Fatalf("callback reply timeout = %v, want %v", got, shortReplyWindow)
	}
}
