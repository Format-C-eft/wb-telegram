package bot

import (
	"context"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// TestCommandQueueExpires проверяет, что ожидание MQTT не задерживает справку и не оживляет старую команду.
func TestCommandQueueExpires(t *testing.T) {
	t.Parallel()

	clock := &movableClock{now: testNow}
	b, deps := newTestBot(t, withClock(clock.get))
	t.Cleanup(func() { _ = b.Stop() })

	publishing := make(chan struct{})
	release := make(chan struct{})

	deps.publisher.EXPECT().Connected().Return(true).Times(2)
	deps.publisher.EXPECT().SetValue(gomock.Any(), "cmd_gate", gomock.Any()).DoAndReturn(func(ctx context.Context, _, _ string) error {
		close(publishing)

		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	deps.sender.EXPECT().Send(int64(100), b.helpText("Супруга")).Return(nil)
	deps.sender.EXPECT().Send(int64(100), "Команда /boiler устарела (отправлена 14:05), повторите").Return(nil)

	b.HandleMessage(t.Context(), message(1, 100, "/gate"))
	awaitSignal(t, publishing)

	b.HandleMessage(t.Context(), message(2, 100, "/boiler"))
	b.HandleMessage(t.Context(), message(3, 100, "/help"))

	clock.advance(11 * time.Second)
	close(release)
	waitForCommandWorker(t, b)
}

// TestDeliveryFlagDoesNotBlockUpdate проверяет, что публикация флага не задерживает обработку Telegram.
func TestDeliveryFlagDoesNotBlockUpdate(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	t.Cleanup(func() { _ = b.Stop() })

	publishing := make(chan struct{})
	release := make(chan struct{})

	deps.publisher.EXPECT().SetError(gomock.Any(), "send", "w").DoAndReturn(func(ctx context.Context, _, _ string) error {
		close(publishing)

		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	deps.publisher.EXPECT().SetError(gomock.Any(), "send", "").Return(nil)

	handled := make(chan struct{})

	go func() {
		b.ReportDelivery(telegram.ErrQueueFull)
		close(handled)
	}()

	awaitSignal(t, publishing)
	awaitSignal(t, handled)

	b.ReportDelivery(nil)
	close(release)
	waitForDeliveryFlag(t, b)
}

// TestCommandQueueFull проверяет отказ при переполнении очереди.
func TestCommandQueueFull(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	t.Cleanup(func() { _ = b.Stop() })

	publishing := make(chan struct{})

	deps.publisher.EXPECT().Connected().Return(true).Times(maxQueuedCommands + 2)
	deps.publisher.EXPECT().SetValue(gomock.Any(), "cmd_gate", gomock.Any()).DoAndReturn(func(ctx context.Context, _, _ string) error {
		close(publishing)
		<-ctx.Done()

		return ctx.Err()
	})
	deps.sender.EXPECT().Send(int64(100), commandBusyText).Return(nil)

	b.HandleMessage(t.Context(), message(1, 100, "/gate"))
	awaitSignal(t, publishing)

	for id := range maxQueuedCommands + 1 {
		b.HandleMessage(t.Context(), message(int64(id+2), 100, "/gate"))
	}

	errStop := b.Stop()
	if errStop != nil {
		t.Fatal(errStop)
	}
}

// TestReplyBeforePublishConfirmation проверяет учёт ответа, пришедшего раньше PUBACK.
func TestReplyBeforePublishConfirmation(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	t.Cleanup(func() { _ = b.Stop() })

	deps.publisher.EXPECT().Connected().Return(true)
	deps.publisher.EXPECT().SetValue(gomock.Any(), "cmd_gate", gomock.Any()).DoAndReturn(func(_ context.Context, _, _ string) error {
		b.HandleSend([]byte(`{"reply_to":"1","text":"Открыто"}`))

		return nil
	})
	deps.sender.EXPECT().Send(int64(100), "Открыто").Return(nil)
	deps.publisher.EXPECT().SetValue(gomock.Any(), "send", "Открыто").Return(nil)

	b.HandleMessage(t.Context(), message(1, 100, "/gate"))
	waitForCommandWorker(t, b)

	b.statsMu.Lock()
	defer b.statsMu.Unlock()

	if b.stats.lastCall == nil || !b.stats.lastCall.replied {
		t.Fatal("reply before PUBACK not recorded")
	}
}
