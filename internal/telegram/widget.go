package telegram

import (
	"context"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const maxWidgetData = 1024

// WidgetQuery содержит запрос виджета из личного чата.
type WidgetQuery struct {
	UpdateID  int64
	QueryID   string
	ChatID    int64
	MessageID int
	Data      string
}

// widgetQueryFromUpdate выделяет запрос данных из личного чата с ботом.
func widgetQueryFromUpdate(update *models.Update) (WidgetQuery, bool) {
	query := update.WidgetQuery
	if query.Chat == nil || query.Chat.Type != models.ChatTypePrivate || query.Chat.ID != query.From.ID ||
		query.Chat.ID <= 0 || query.ID == "" || query.MessageID <= 0 || query.Type.Type != models.WidgetQueryTypeTypeData {
		return WidgetQuery{}, false
	}

	return WidgetQuery{UpdateID: update.ID, QueryID: query.ID, ChatID: query.Chat.ID, MessageID: query.MessageID, Data: query.Data}, true
}

// AnswerWidgetQuery возвращает результат запроса виджета.
func (c *Client) AnswerWidgetQuery(ctx context.Context, queryID, data string) error {
	if queryID == "" || len(data) > maxWidgetData {
		return ErrInvalidArgument
	}

	return c.runControlRequest(ctx, "answer widget query", func(requestCtx context.Context) error {
		_, errAnswer := c.api.AnswerWidgetQuery(requestCtx, &bot.AnswerWidgetQueryParams{WidgetQueryID: queryID, Data: data})

		return errAnswer
	})
}

// EditWidgetData сохраняет данные в исходном сообщении виджета.
func (c *Client) EditWidgetData(ctx context.Context, query WidgetQuery, data string) error {
	if query.ChatID <= 0 || query.MessageID <= 0 || len(data) > maxWidgetData {
		return ErrInvalidArgument
	}

	return c.runControlRequest(ctx, "edit widget data", func(requestCtx context.Context) error {
		_, errEdit := c.api.EditMessageWidgetData(requestCtx, &bot.EditMessageWidgetDataParams{ChatID: query.ChatID, MessageID: query.MessageID, ChatData: &data})

		return errEdit
	})
}
