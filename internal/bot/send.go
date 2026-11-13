package bot

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/Format-C-eft/wb-telegram/internal/chart"
	"github.com/Format-C-eft/wb-telegram/internal/telegram"

	"github.com/Format-C-eft/wb-telegram/internal/wbdevice"
)

// sendRequest описывает сообщение или запрос графика от правил.
type sendRequest struct {
	ID        json.RawMessage    `json:"id"`
	Text      string             `json:"text"`
	To        []string           `json:"to"`
	ReplyTo   string             `json:"reply_to"`
	ParseMode telegram.ParseMode `json:"parse_mode"`
	Buttons   [][]buttonSpec     `json:"buttons"`
	Chart     *chart.Spec        `json:"chart"`
	Ephemeral bool               `json:"ephemeral"`
	Boiler    *boilerWidget      `json:"boiler_widget"`
}

// HandleSend принимает сообщение правил без ожидания построения графика.
func (b *Bot) HandleSend(payload []byte) {
	if !b.beginOperation() {
		return
	}
	defer b.operations.Done()

	req, errParse := parseSendRequest(fixCESU8(payload))
	if errParse != nil {
		slog.Warn("send: сообщение правил отклонено", "err", errParse)

		return
	}

	if req.Chart != nil {
		b.handleChart(req)

		return
	}

	if req.Boiler != nil {
		b.handleBoilerWidget(req.ReplyTo, *req.Boiler)

		return
	}

	if req.Ephemeral {
		target := b.takeEphemeralReply(req.ReplyTo)
		b.deliverEphemeral(target, telegram.Delivery{Kind: telegram.DeliveryText, Text: req.Text, ParseMode: req.ParseMode})

		return
	}

	for _, chatID := range b.recipients(req) {
		if req.ParseMode == telegram.ParseModePlain && req.Buttons == nil {
			b.deliver(chatID, req.Text)
		} else {
			b.sendInteraction(chatID, req)
		}
	}

	b.publishSummary(req.Text)
}

// publishSummary сохраняет текст последнего сообщения в контроле send.
func (b *Bot) publishSummary(text string) {
	errValue := b.publisher.SetValue(b.operationCtx, SendControl, text)
	if errValue != nil {
		slog.Warn("MQTT: текст сообщения не опубликован", "err", errValue)
	}
}

// reportInteractionError записывает локальный отказ без повторной отправки через закрытый клиент.
func (b *Bot) reportInteractionError(chatID int64, err error) {
	slog.Warn("Telegram: сообщение не принято", "chat_id", chatID, "err", err)

	if errors.Is(err, telegram.ErrClosed) {
		return
	}

	b.deliver(chatID, "Не удалось подготовить сообщение, повторите команду")
}

// ReportDelivery отражает результат отправки во флаге контрола send.
func (b *Bot) ReportDelivery(err error) {
	if !b.beginOperation() {
		return
	}

	failing := err != nil

	b.deliveryMu.Lock()
	defer b.deliveryMu.Unlock()

	if failing == b.deliveryFailing {
		b.operations.Done()

		return
	}

	b.deliveryFailing = failing

	if b.deliveryDone != nil {
		b.operations.Done()

		return
	}

	b.deliveryDone = make(chan struct{})

	go b.publishDeliveryFlag(failing)
}

// publishDeliveryFlag публикует состояние доставки без задержки обработчика Telegram.
func (b *Bot) publishDeliveryFlag(failing bool) {
	defer b.operations.Done()

	for {
		flags := ""
		if failing {
			flags = wbdevice.ErrorFlagWrite
		}

		errSet := b.publisher.SetError(b.operationCtx, SendControl, flags)
		if errSet != nil && b.operationCtx.Err() == nil {
			slog.Warn("MQTT: не удалось обновить флаг ошибки send", "err", errSet)
		}

		b.deliveryMu.Lock()

		if failing == b.deliveryFailing || b.operationCtx.Err() != nil {
			close(b.deliveryDone)
			b.deliveryDone = nil
			b.deliveryMu.Unlock()

			return
		}

		failing = b.deliveryFailing

		b.deliveryMu.Unlock()
	}
}

// badPayload создаёт безопасную ошибку входного сообщения.
func badPayload(message string) error {
	return &Error{kind: kindBadPayload, Op: "parse send", Message: message}
}

// parseSendRequest разбирает и проверяет сообщение правил.
func parseSendRequest(payload []byte) (sendRequest, error) {
	trimmed := bytes.TrimSpace(payload)

	var req sendRequest
	if bytes.HasPrefix(trimmed, []byte("{")) {
		errUnmarshal := json.Unmarshal(trimmed, &req)
		if errUnmarshal != nil {
			return sendRequest{}, badPayload("invalid JSON fields")
		}
	} else {
		req.Text = string(trimmed)
	}

	errValidate := req.validate()
	if errValidate != nil {
		return sendRequest{}, errValidate
	}

	if req.ParseMode == telegram.ParseModePlain {
		req.Text = strings.TrimSpace(req.Text)
	}

	return req, nil
}

// validate проверяет сочетания полей сообщения.
func (req sendRequest) validate() error {
	if req.Ephemeral && (req.ReplyTo == "" || len(req.To) != 0 || req.Buttons != nil || req.Boiler != nil) {
		return badPayload("ephemeral message requires a button reply without recipients or buttons")
	}

	if req.Boiler != nil {
		return req.validateBoilerReply()
	}

	if req.Chart != nil {
		return req.validateChartReply()
	}

	if strings.TrimSpace(req.Text) == "" {
		return badPayload("empty text")
	}

	if req.ParseMode != telegram.ParseModePlain && req.ParseMode != telegram.ParseModeHTML && req.ParseMode != telegram.ParseModeMarkdownV2 {
		return badPayload("unsupported parse mode")
	}

	if req.ParseMode != telegram.ParseModePlain && len(utf16.Encode([]rune(req.Text))) > 4096 {
		return badPayload("formatted text is too long")
	}

	if req.Buttons != nil {
		return validateButtons(req.Buttons)
	}

	return nil
}

// validateChartReply проверяет поля запроса графика.
func (req sendRequest) validateChartReply() error {
	if req.Text != "" || req.ParseMode != "" || req.Buttons != nil {
		return badPayload("chart cannot contain text, formatting or buttons")
	}

	errSpec := req.Chart.Validate()
	if errSpec != nil {
		return badPayload("invalid chart specification")
	}

	return nil
}

// recipients выбирает chat_id получателей: автор вызова, названные люди или подписчики уведомлений.
func (b *Bot) recipients(req sendRequest) []int64 {
	if req.ReplyTo != "" {
		return b.replyRecipient(req.ReplyTo)
	}

	var chats []int64

	if len(req.To) > 0 {
		for _, name := range req.To {
			user, ok := b.cfg.UserByName(name)
			if !ok {
				slog.Warn("send: неизвестный получатель", "user", name)

				continue
			}

			if !slices.Contains(chats, user.ChatID) {
				chats = append(chats, user.ChatID)
			}
		}

		return chats
	}

	for _, user := range b.cfg.NotificationUsers() {
		chats = append(chats, user.ChatID)
	}

	return chats
}

// replyRecipient находит автора вызова и отмечает получение ответа.
func (b *Bot) replyRecipient(id string) []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	call := b.pendingReply(id)
	if call == nil {
		slog.Warn("send: ответ на неизвестный или слишком старый вызов", "reply_to", id)

		return nil
	}

	if call.source.widget != nil {
		return nil
	}

	b.completeReply(id, call)

	return []int64{call.chatID}
}
