package bot

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// TestBoilerWidgetReplyToGroup проверяет текстовый ответ в настроенной группе.
func TestBoilerWidgetReplyToGroup(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	t.Cleanup(func() { _ = b.Stop() })

	if !b.track("50", -100, boilerWidgetCommand, testNow, commandSource{}) {
		t.Fatal("command was not tracked")
	}

	deps.sender.EXPECT().Send(int64(-100), "Котёл: 52 °C, Ошибок нет").Return(nil)
	b.HandleSend([]byte(`{"reply_to":"50","boiler_widget":{"temperature":"52 °C","status":"Ошибок нет"}}`))
}

// TestWidgetReplyDoesNotBlockMQTT проверяет независимость уведомлений от запроса Telegram.
func TestWidgetReplyDoesNotBlockMQTT(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	t.Cleanup(func() { _ = b.Stop() })

	query := telegram.WidgetQuery{QueryID: "refresh", ChatID: 100, MessageID: 1}
	if !b.track("50", query.ChatID, boilerWidgetCommand, testNow, commandSource{widget: &query}) {
		t.Fatal("command was not tracked")
	}

	started := make(chan struct{})

	deps.sender.EXPECT().AnswerWidgetQuery(gomock.Any(), query.QueryID, gomock.Any()).
		DoAndReturn(func(ctx context.Context, _ string, _ string) error {
			close(started)
			<-ctx.Done()

			return ctx.Err()
		})

	deps.sender.EXPECT().Send(int64(100), "Котёл: авария").Return(nil)
	deps.publisher.EXPECT().SetValue(gomock.Any(), "send", "Котёл: авария").Return(nil)

	dispatched := make(chan struct{})

	go func() {
		b.HandleSend([]byte(`{"reply_to":"50","boiler_widget":{"temperature":"52 °C","status":"Ошибок нет"}}`))
		b.HandleSend([]byte(`{"text":"Котёл: авария"}`))
		close(dispatched)
	}()

	awaitSignal(t, started)
	awaitSignal(t, dispatched)
}

// TestWidgetReplyDeadline проверяет отказ на границе срока до срабатывания таймера.
func TestWidgetReplyDeadline(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	t.Cleanup(func() { _ = b.Stop() })

	query := telegram.WidgetQuery{QueryID: "refresh", ChatID: 100, MessageID: 1}

	created := testNow.Add(-b.cfg.ReplyTimeout())
	if !b.track("50", query.ChatID, boilerWidgetCommand, created, commandSource{widget: &query}) {
		t.Fatal("command was not tracked")
	}

	sent := make(chan struct{})

	deps.sender.EXPECT().AnswerWidgetQuery(gomock.Any(), query.QueryID, `{"error":"Правило не ответило вовремя"}`).
		DoAndReturn(func(ctx context.Context, _ string, _ string) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("widget response has no deadline")
			}

			close(sent)

			return nil
		})

	b.startReplyTimer("50")

	if got := deps.timers.duration(0); got != 0 {
		t.Fatalf("widget reply timeout = %v, want 0", got)
	}

	b.HandleSend([]byte(`{"reply_to":"50","boiler_widget":{"temperature":"52 °C","status":"Ошибок нет"}}`))
	deps.timers.fire(0)
	awaitSignal(t, sent)
}
