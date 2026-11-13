package bot

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

const boilerWidgetCommand = "boiler_status"

//go:embed boiler_widget.html
var boilerWidgetHTML string

// boilerWidget содержит отображаемые показания котла.
type boilerWidget struct {
	Temperature string `json:"temperature"`
	Status      string `json:"status"`
	UpdatedAt   string `json:"updated_at"`
}

// validateBoilerReply проверяет адрес и показания карточки.
func (req sendRequest) validateBoilerReply() error {
	if req.ReplyTo == "" || len(req.To) != 0 || req.Text != "" || req.ParseMode != "" || req.Buttons != nil || req.Chart != nil {
		return badPayload("boiler widget requires a standalone reply")
	}

	w := req.Boiler
	if strings.TrimSpace(w.Temperature) == "" || strings.TrimSpace(w.Status) == "" || len(w.Temperature) > 48 || len(w.Status) > 96 {
		return badPayload("invalid boiler widget readings")
	}

	return nil
}

// HandleWidgetQuery запрашивает свежие показания у правила котла.
func (b *Bot) HandleWidgetQuery(_ context.Context, query telegram.WidgetQuery) {
	if !b.beginOperation() {
		return
	}
	defer b.operations.Done()

	user, known := b.cfg.UserByChatID(query.ChatID)
	command, configured := b.cfg.Command(boilerWidgetCommand)

	now := b.options.now()
	deadline := now.Add(shortReplyWindow)

	if query.Data != "refresh" || !known || !configured || !b.cfg.Allowed(command, user.Name) {
		b.answerWidgetError(query, deadline, "Обновление недоступно")

		return
	}

	if !b.publisher.Connected() {
		b.answerWidgetError(query, deadline, "Контроллер недоступен, показания не обновлены")

		return
	}

	if !b.queueCommand(commandJob{updateID: query.UpdateID, chatID: query.ChatID, user: user.Name, name: command.Name, received: now, source: commandSource{widget: &query}}) {
		b.answerWidgetError(query, deadline, commandBusyText)
	}
}

// takeBoilerReply связывает показания с вызовом команды и завершает ожидание.
func (b *Bot) takeBoilerReply(id string) (int64, *telegram.WidgetQuery, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	call := b.pendingReply(id)
	if call == nil || b.stopped || call.command != boilerWidgetCommand {
		return 0, nil, time.Time{}
	}

	if call.answered || (call.source.widget != nil && (call.timeoutNotified || !b.options.now().Before(call.replyDeadline))) {
		return 0, nil, time.Time{}
	}

	b.completeReply(id, call)

	return call.chatID, call.source.widget, call.createdAt.Add(shortReplyWindow)
}

// handleBoilerWidget отправляет карточку или обновляет данные существующей.
func (b *Bot) handleBoilerWidget(id string, widget boilerWidget) {
	chatID, query, deadline := b.takeBoilerReply(id)
	if chatID == 0 {
		return
	}

	if chatID < 0 {
		b.deliver(chatID, "Котёл: "+widget.Temperature+", "+widget.Status)

		return
	}

	widget.UpdatedAt = b.options.now().In(b.options.location).Format("02.01 15:04:05")

	data, errMarshal := json.Marshal(widget)
	if errMarshal != nil {
		slog.Error("не удалось закодировать данные котла", "err", errMarshal)

		return
	}

	if query != nil {
		b.refreshWidget(*query, deadline, string(data))

		return
	}

	caption := "Котёл при открытии: " + widget.Temperature + ", " + widget.Status
	delivery := telegram.Delivery{Kind: telegram.DeliveryWidget, Caption: caption, Widget: telegram.Widget{HTML: boilerWidgetHTML, Data: string(data)}}

	errSend := b.sender.SendDelivery(chatID, delivery)
	if errSend != nil && !errors.Is(errSend, telegram.ErrQueueFull) {
		b.reportInteractionError(chatID, errSend)
	}
}

// refreshWidget отвечает клиенту и сохраняет свежие данные в сообщении.
func (b *Bot) refreshWidget(query telegram.WidgetQuery, deadline time.Time, data string) {
	b.queueShortReply(shortReply{deadline: deadline, widget: widgetMessage{chatID: query.ChatID, messageID: query.MessageID}, send: func(ctx context.Context) {
		if !b.answerWidget(ctx, query, data) {
			return
		}

		editCtx, cancel := context.WithTimeout(b.operationCtx, shortSendBudget)
		defer cancel()

		errEdit := b.sender.EditWidgetData(editCtx, query, data)
		if errEdit != nil {
			slog.Debug("Telegram: состояние виджета не сохранено", "err", errEdit)
		}
	}})
}

// answerWidgetError показывает ошибку внутри виджета.
func (b *Bot) answerWidgetError(query telegram.WidgetQuery, deadline time.Time, message string) {
	data, errMarshal := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: message})
	if errMarshal != nil {
		return
	}

	b.queueShortReply(shortReply{deadline: deadline, send: func(ctx context.Context) {
		b.answerWidget(ctx, query, string(data))
	}})
}

// answerWidget передаёт результат запроса в Telegram.
func (b *Bot) answerWidget(ctx context.Context, query telegram.WidgetQuery, data string) bool {
	errAnswer := b.sender.AnswerWidgetQuery(ctx, query.QueryID, data)
	if errAnswer != nil {
		slog.Debug("Telegram: ответ виджету не отправлен", "err", errAnswer)

		return false
	}

	return true
}
