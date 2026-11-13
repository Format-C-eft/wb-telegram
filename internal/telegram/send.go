package telegram

import (
	"bytes"
	"context"
	"log/slog"

	"github.com/go-telegram/bot/models"

	"github.com/go-telegram/bot"

	"github.com/Format-C-eft/wb-telegram/internal/retry"
)

// MaxMessageRunes ограничивает длину текста единицами UTF-16.
const MaxMessageRunes = 4096

// Send ставит обычный текст в очередь отправки.
func (c *Client) Send(chatID int64, text string) error {
	return c.SendDelivery(chatID, Delivery{Kind: DeliveryText, Text: text})
}

// SendDelivery ставит доставку в очередь с копированием её данных.
func (c *Client) SendDelivery(chatID int64, delivery Delivery) error {
	errValidate := delivery.validate()
	if errValidate != nil {
		return errValidate
	}

	if delivery.Kind == DeliveryText && delivery.ParseMode == ParseModePlain {
		delivery.Text = truncate(delivery.Text)
	}

	dropped, ok := c.queue.pushDelivery(outgoing{chatID: chatID, text: delivery.Text, delivery: delivery.clone()})
	if !ok {
		return ErrClosed
	}

	if dropped > 0 {
		c.dropped.Add(uint64(dropped))
		slog.Warn("Telegram: очередь отправки переполнена", "dropped", dropped)

		for range dropped {
			c.options.onDelivery(ErrQueueFull)
		}

		return ErrQueueFull
	}

	return nil
}

// truncate ограничивает plain текст единицами UTF-16.
func truncate(text string) string {
	if utf16Length(text) <= MaxMessageRunes {
		return text
	}

	units := 0
	end := 0

	for index, char := range text {
		size := 1
		if char > 0xffff {
			size = 2
		}

		if units+size > MaxMessageRunes-1 {
			break
		}

		units += size
		end = index + len(string(char))
	}

	return text[:end] + "…"
}

// sendAttempt отправляет одну попытку с новым reader изображения.
func (c *Client) sendAttempt(ctx context.Context, item outgoing, callbackID string) error {
	d := item.delivery

	var (
		chatID    any = item.chatID
		ephemeral *models.EphemeralMessageParameters
	)

	if callbackID != "" {
		chatID = nil
		ephemeral = &models.EphemeralMessageParameters{CallbackQueryID: callbackID}
	}

	switch d.Kind {
	case DeliveryWidget:
		_, errWidget := c.api.SendWidget(ctx, &bot.SendWidgetParams{ChatID: chatID, HTML: d.Widget.HTML, ChatData: d.Widget.Data, Caption: d.Caption, AllowQueries: true})

		return errWidget
	case DeliveryPhoto:
		_, errPhoto := c.api.SendPhoto(ctx, &bot.SendPhotoParams{
			ChatID: chatID, Photo: &models.InputFileUpload{Filename: "chart.png", Data: bytes.NewReader(d.PNG)},
			Caption: d.Caption, ParseMode: models.ParseMode(d.ParseMode), EphemeralMessageParameters: ephemeral,
		})

		return errPhoto
	}

	params := &bot.SendMessageParams{ChatID: chatID, Text: item.text, ParseMode: models.ParseMode(d.ParseMode), EphemeralMessageParameters: ephemeral}
	if len(d.Keyboard) > 0 {
		rows := make([][]models.InlineKeyboardButton, len(d.Keyboard))
		for index, row := range d.Keyboard {
			for _, button := range row {
				rows[index] = append(rows[index], models.InlineKeyboardButton{Text: button.Text, CallbackData: button.Data})
			}
		}

		params.ReplyMarkup = &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	}

	_, errMessage := c.api.SendMessage(ctx, params)

	return errMessage
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

// deliver доставляет сообщение с лимитами и повторами временных ошибок.
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

		errSend := c.sendAttempt(ctx, item, "")
		c.limiter.record(item.chatID)

		if errSend == nil {
			c.sent.Add(1)
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
			c.dropped.Add(1)
			c.markHealthy()
			slog.Warn("Telegram: сообщение отклонено, не доставлено", "chat_id", item.chatID, "err", failure.Message)

			return true
		}

		// Прочие неповторяемые ошибки - сообщение выбрасывается.
		if failure.kind != kindRateLimited && failure.kind != kindTransient {
			c.dropped.Add(1)

			return true
		}

		pause := max(failure.RetryAfter, retry.Delay(attempt, c.options.retryBase, c.options.retryMax))

		if !c.options.sleep(ctx, pause) {
			return false
		}
	}
}

// dropAfterFatal выбрасывает сообщение после отклонения токена.
func (c *Client) dropAfterFatal() {
	c.dropped.Add(1)

	if c.fatalReported {
		return
	}

	c.fatalReported = true
	c.options.onDelivery(ErrUnauthorized)
}
