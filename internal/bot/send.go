package bot

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/Format-C-eft/wb-telegram/internal/wbdevice"
)

// sendRequest — сообщение от правил в контрол send; поле id ботом игнорируется.
type sendRequest struct {
	Text    string   `json:"text"`
	To      []string `json:"to"`
	ReplyTo string   `json:"reply_to"`
}

// HandleSend доставляет сообщение правил получателям и публикует его в контрол send.
func (b *Bot) HandleSend(payload []byte) {
	if b.isStopped() {
		slog.Debug("ядро остановлено, сообщение правил не отправлено")

		return
	}

	req, errParse := parseSendRequest(payload)
	if errParse != nil {
		slog.Warn("send: сообщение от правил проигнорировано", "err", errParse, "payload", string(payload))

		return
	}

	for _, chatID := range b.recipients(req) {
		b.deliver(chatID, req.Text)
	}

	errValue := b.publisher.SetValue(SendControl, req.Text)
	if errValue != nil {
		slog.Warn("MQTT: не удалось обновить контрол send", "err", errValue)
	}
}

// ReportDelivery отражает результат доставки во флаге ошибки контрола send: "w" после неудачной
// попытки, "" после первой успешной; публикует только при смене состояния. Вызывается из любых
// горутин клиента Telegram. Публикация идёт под deliveryMu, поэтому флаги в MQTT меняются в том же
// порядке, что и состояние; SetError устройства не вызывает ядро обратно, взаимоблокировки нет.
func (b *Bot) ReportDelivery(err error) {
	failing := err != nil

	b.deliveryMu.Lock()
	defer b.deliveryMu.Unlock()

	if failing == b.deliveryFailing {
		return
	}

	b.deliveryFailing = failing

	flags := ""
	if failing {
		flags = wbdevice.ErrorFlagWrite
	}

	errSet := b.publisher.SetError(SendControl, flags)
	if errSet != nil {
		slog.Warn("MQTT: не удалось обновить флаг ошибки send", "err", errSet)
	}
}

// parseSendRequest разбирает строку или JSON-объект; пустой текст — ошибка.
func parseSendRequest(payload []byte) (sendRequest, error) {
	trimmed := bytes.TrimSpace(payload)

	var req sendRequest

	if bytes.HasPrefix(trimmed, []byte("{")) {
		errUnmarshal := json.Unmarshal(trimmed, &req)
		if errUnmarshal != nil {
			return sendRequest{}, &Error{kind: kindBadPayload, Op: "parse send", Message: errUnmarshal.Error()}
		}
	} else {
		req.Text = string(trimmed)
	}

	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		return sendRequest{}, &Error{kind: kindBadPayload, Op: "parse send", Message: "empty text"}
	}

	return req, nil
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

// replyRecipient находит автора вызова и снимает ожидание ответа; вызов остаётся в памяти,
// чтобы повторные и поздние ответы тоже дошли до автора.
func (b *Bot) replyRecipient(id string) []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	call, ok := b.pending[id]
	if !ok {
		slog.Warn("send: ответ на неизвестный или слишком старый вызов", "reply_to", id)

		return nil
	}

	call.answered = true
	call.stop()

	return []int64{call.chatID}
}
