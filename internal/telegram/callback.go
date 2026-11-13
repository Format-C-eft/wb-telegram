package telegram

import (
	"context"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Callback содержит данные нажатия без типов SDK.
type Callback struct {
	UpdateID   int64
	QueryID    string
	FromID     int64
	ChatID     int64
	ChatType   string
	MessageID  int
	Data       string
	Accessible bool
}

// CallbackHandler обрабатывает нажатие кнопки.
type CallbackHandler func(context.Context, Callback)

// callbackFromUpdate преобразует нажатие с проверкой доступности сообщения.
func callbackFromUpdate(update *models.Update) Callback {
	query := update.CallbackQuery
	callback := Callback{UpdateID: update.ID, QueryID: query.ID, FromID: query.From.ID, Data: query.Data}

	switch query.Message.Type {
	case models.MaybeInaccessibleMessageTypeMessage:
		message := query.Message.Message
		if message != nil {
			callback.ChatID = message.Chat.ID
			callback.ChatType = string(message.Chat.Type)
			callback.MessageID = message.ID
			callback.Accessible = true
		}
	case models.MaybeInaccessibleMessageTypeInaccessibleMessage:
		message := query.Message.InaccessibleMessage
		if message != nil {
			callback.ChatID = message.Chat.ID
			callback.ChatType = string(message.Chat.Type)
			callback.MessageID = message.MessageID
		}
	}

	return callback
}

// runControlRequest выполняет управляющий запрос с ограничением параллелизма.
func (c *Client) runControlRequest(ctx context.Context, op string, request func(context.Context) error) error {
	if ctx.Err() != nil {
		return translate(op, ctx.Err(), c.mask)
	}

	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()

	if closed {
		return ErrClosed
	}

	select {
	case c.control <- struct{}{}:
	case <-ctx.Done():
		return ErrBusy
	}

	defer func() { <-c.control }()

	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	errRequest := request(requestCtx)
	if errRequest != nil {
		if ctx.Err() != nil {
			return translate(op, errRequest, c.mask)
		}

		return c.fail(op, errRequest)
	}

	c.markHealthy()

	return nil
}

// AnswerCallback подтверждает нажатие вне очереди сообщений.
func (c *Client) AnswerCallback(ctx context.Context, queryID, text string, alert bool) error {
	if queryID == "" {
		return ErrInvalidArgument
	}

	return c.runControlRequest(ctx, "answer callback", func(requestCtx context.Context) error {
		_, errAnswer := c.api.AnswerCallbackQuery(requestCtx, &bot.AnswerCallbackQueryParams{CallbackQueryID: queryID, Text: text, ShowAlert: alert})

		return errAnswer
	})
}

// SendEphemeral отправляет временный ответ на кнопку в обход очереди.
func (c *Client) SendEphemeral(ctx context.Context, queryID string, delivery Delivery) error {
	if queryID == "" || len(delivery.Keyboard) != 0 || (delivery.Kind != DeliveryText && delivery.Kind != DeliveryPhoto) {
		return ErrInvalidArgument
	}

	errValidate := delivery.validate()
	if errValidate != nil {
		return errValidate
	}

	if delivery.Kind == DeliveryText && delivery.ParseMode == ParseModePlain {
		delivery.Text = truncate(delivery.Text)
	}

	return c.runControlRequest(ctx, "send ephemeral", func(requestCtx context.Context) error {
		return c.sendAttempt(requestCtx, outgoing{text: delivery.Text, delivery: delivery}, queryID)
	})
}

// ClearKeyboard снимает кнопки исходного сообщения вне очереди.
func (c *Client) ClearKeyboard(ctx context.Context, chatID int64, messageID int) error {
	if chatID == 0 || messageID <= 0 {
		return ErrInvalidArgument
	}

	return c.runControlRequest(ctx, "clear keyboard", func(requestCtx context.Context) error {
		_, errClear := c.api.EditMessageReplyMarkup(requestCtx, &bot.EditMessageReplyMarkupParams{ChatID: chatID, MessageID: messageID, ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}})

		return errClear
	})
}
