package bot

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

const (
	shortReplyWindow = 13 * time.Second
	shortSendBudget  = 2 * time.Second
	maxShortReplies  = 8
	maxShortWorkers  = 3
)

// widgetMessage идентифицирует сообщение виджета.
type widgetMessage struct {
	chatID    int64
	messageID int
}

// shortReply хранит ограниченный по времени запрос к Telegram.
type shortReply struct {
	deadline time.Time
	widget   widgetMessage
	callback string
	send     func(context.Context)
}

// queueShortReply ставит ответ в ограниченную очередь Telegram.
func (b *Bot) queueShortReply(reply shortReply) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.queueShortReplyLocked(reply)
}

// queueShortReplyLocked ставит ответ в очередь под b.mu.
func (b *Bot) queueShortReplyLocked(reply shortReply) {
	if b.stopped {
		return
	}

	if len(b.shortReplies) >= maxShortReplies {
		slog.Warn("Telegram: очередь коротких ответов заполнена")

		return
	}

	b.shortReplies = append(b.shortReplies, reply)
	b.startShortWorker()
}

// startShortWorker запускает готовый запрос под b.mu.
func (b *Bot) startShortWorker() {
	if b.stopped || b.shortWorkers >= maxShortWorkers {
		return
	}

	for index, reply := range b.shortReplies {
		if b.shortActive[reply.widget] || b.callbackActive[reply.callback] {
			continue
		}

		if reply.widget != (widgetMessage{}) {
			b.shortActive[reply.widget] = true
		}

		if reply.callback != "" {
			b.callbackActive[reply.callback] = true
		}

		b.shortReplies = slices.Delete(b.shortReplies, index, index+1)
		b.shortWorkers++
		b.operations.Add(1)

		go b.sendShortReply(reply)

		return
	}
}

// sendShortReply выполняет запрос вне обработчика MQTT.
func (b *Bot) sendShortReply(reply shortReply) {
	defer b.operations.Done()
	defer func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		delete(b.shortActive, reply.widget)
		delete(b.callbackActive, reply.callback)
		b.shortWorkers--
		b.startShortWorker()
	}()

	remaining := reply.deadline.Sub(b.options.now())
	if remaining <= 0 {
		slog.Warn("Telegram: истёк срок короткого ответа")

		return
	}

	ctx, cancel := context.WithTimeout(b.operationCtx, remaining)
	defer cancel()

	reply.send(ctx)
}
