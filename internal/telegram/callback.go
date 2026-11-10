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

// controlRequest ограничивает длительность и параллелизм управляющих запросов.
func (c *Client) controlRequest(ctx context.Context, op string, request func(context.Context) error) error {
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
		defer func() { <-c.control }()
	default:
		return ErrBusy
	}

	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	errRequest := request(requestCtx)
	if errRequest != nil {
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

	return c.controlRequest(ctx, "answer callback", func(requestCtx context.Context) error {
		_, errAnswer := c.api.AnswerCallbackQuery(requestCtx, &bot.AnswerCallbackQueryParams{CallbackQueryID: queryID, Text: text, ShowAlert: alert})

		return errAnswer
	})
}

// ClearKeyboard снимает кнопки исходного сообщения вне очереди.
func (c *Client) ClearKeyboard(ctx context.Context, chatID int64, messageID int) error {
	if chatID == 0 || messageID <= 0 {
		return ErrInvalidArgument
	}

	return c.controlRequest(ctx, "clear keyboard", func(requestCtx context.Context) error {
		_, errClear := c.api.EditMessageReplyMarkup(requestCtx, &bot.EditMessageReplyMarkupParams{ChatID: chatID, MessageID: messageID, ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}})

		return errClear
	})
}
