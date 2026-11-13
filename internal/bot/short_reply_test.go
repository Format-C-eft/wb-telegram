package bot

import (
	"context"
	"testing"
)

// TestShortRepliesPreserveWidgetOrder проверяет порядок обновлений одной карточки.
func TestShortRepliesPreserveWidgetOrder(t *testing.T) {
	t.Parallel()

	b, _ := newTestBot(t)
	t.Cleanup(func() { _ = b.Stop() })

	first := make(chan struct{})
	unblock := make(chan struct{})
	same := make(chan struct{})
	other := make(chan struct{})
	deadline := testNow.Add(shortReplyWindow)
	widget := widgetMessage{chatID: 100, messageID: 1}

	b.queueShortReply(shortReply{deadline: deadline, widget: widget, send: func(ctx context.Context) {
		close(first)

		select {
		case <-unblock:
		case <-ctx.Done():
		}
	}})

	awaitSignal(t, first)

	b.queueShortReply(shortReply{deadline: deadline, widget: widget, send: func(context.Context) { close(same) }})
	b.queueShortReply(shortReply{deadline: deadline, send: func(context.Context) { close(other) }})

	awaitSignal(t, other)

	select {
	case <-same:
		t.Fatal("second update started before the first finished")
	default:
	}

	close(unblock)
	awaitSignal(t, same)
}
