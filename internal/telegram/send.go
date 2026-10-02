package telegram

import (
	"context"
	"log/slog"
	"unicode/utf8"

	"github.com/go-telegram/bot"

	"github.com/Format-C-eft/wb-telegram/internal/retry"
)

// MaxMessageRunes — максимальная длина сообщения Telegram в символах.
const MaxMessageRunes = 4096

// Send ставит сообщение в очередь отправки. При переполнении самое старое сообщение
// выбрасывается (onDelivery получает ErrQueueFull), новое ставится, возвращается ErrQueueFull.
// После остановки клиента возвращает ErrClosed.
func (c *Client) Send(chatID int64, text string) error {
	dropped, ok := c.queue.push(outgoing{chatID: chatID, text: truncate(text)})
	if !ok {
		return ErrClosed
	}

	if dropped {
		slog.Warn("Telegram: очередь отправки переполнена, самое старое сообщение выброшено")
		c.options.onDelivery(ErrQueueFull)

		return ErrQueueFull
	}

	return nil
}

// truncate обрезает текст до MaxMessageRunes символов, заканчивая многоточием.
func truncate(text string) string {
	if utf8.RuneCountInString(text) <= MaxMessageRunes {
		return text
	}

	runes := []rune(text)

	return string(runes[:MaxMessageRunes-1]) + "…"
}

// sendLoop отправляет сообщения из очереди, пока очередь не закрыта и не пуста или не отменён ctx.
func (c *Client) sendLoop(ctx context.Context) {
	for {
		item, ok := c.queue.pop(ctx)
		if !ok {
			return
		}

		if !c.deliver(ctx, item) {
			c.lostInFlight = true

			return
		}
	}
}

// deliver отправляет одно сообщение с соблюдением лимитов, повторяя его при 429 и временных
// ошибках до успеха; окончательный отказ и 401 сообщение выбрасывают. false — ctx отменён.
func (c *Client) deliver(ctx context.Context, item outgoing) bool {
	for attempt := 0; ; attempt++ {
		if c.fatal.Load() {
			c.dropAfterFatal()

			return true
		}

		if wait := c.limiter.delay(item.chatID); wait > 0 {
			if !c.options.sleep(ctx, wait) {
				return false
			}
		}

		c.limiter.record(item.chatID)

		_, errSend := c.api.SendMessage(ctx, &bot.SendMessageParams{ChatID: item.chatID, Text: item.text})
		if errSend == nil {
			c.markHealthy()
			c.options.onDelivery(nil)

			return true
		}

		failure := c.fail("send", errSend)

		if ctx.Err() != nil {
			return false
		}

		if failure.kind == kindUnauthorized {
			c.dropAfterFatal()

			return true
		}

		c.options.onDelivery(failure)

		if failure.kind == kindRejected {
			c.markHealthy()
			slog.Warn("Telegram: сообщение отклонено, не доставлено", "chat_id", item.chatID, "err", failure.Message)

			return true
		}

		// Прочие неповторяемые ошибки — сообщение выбрасывается.
		if failure.kind != kindRateLimited && failure.kind != kindTransient {
			return true
		}

		pause := max(failure.RetryAfter, retry.Delay(attempt, c.options.retryBase, c.options.retryMax))

		if !c.options.sleep(ctx, pause) {
			return false
		}
	}
}

// dropAfterFatal выбрасывает сообщение после отклонения токена: onDelivery(ErrUnauthorized)
// сообщается один раз, а не на каждое сообщение. Вызывается только из цикла отправки.
func (c *Client) dropAfterFatal() {
	if c.fatalReported {
		return
	}

	c.fatalReported = true
	c.options.onDelivery(ErrUnauthorized)
}
