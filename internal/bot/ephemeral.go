package bot

import (
	"context"
	"log/slog"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// ephemeralReply хранит адрес и срок временного ответа.
type ephemeralReply struct {
	queryID  string
	chatID   int64
	deadline time.Time
}

// takeEphemeralReply резервирует единственный временный ответ на нажатие.
func (b *Bot) takeEphemeralReply(id string) ephemeralReply {
	b.mu.Lock()
	defer b.mu.Unlock()

	call := b.pendingReply(id)
	if call == nil || b.stopped || call.source.callbackID == "" || call.ephemeralUsed {
		return ephemeralReply{}
	}

	deadline := call.replyDeadline
	if !b.options.now().Before(deadline) {
		return ephemeralReply{}
	}

	call.ephemeralUsed = true
	b.completeReply(id, call)

	return ephemeralReply{queryID: call.source.callbackID, chatID: call.chatID, deadline: deadline}
}

// deliverEphemeral отправляет временный ответ в пределах окна Telegram.
func (b *Bot) deliverEphemeral(target ephemeralReply, delivery telegram.Delivery) {
	if target.queryID == "" {
		return
	}

	b.queueShortReply(shortReply{deadline: target.deadline, callback: target.queryID, send: func(ctx context.Context) {
		errSend := b.sender.SendEphemeral(ctx, target.queryID, delivery)
		if errSend != nil {
			slog.Debug("Telegram: временный ответ не отправлен", "err", errSend)
		}
	}})
}
