package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// unavailableText — ответ, когда команду не удалось передать контроллеру.
const unavailableText = "Контроллер недоступен, команда не выполнена"

// commandEvent — JSON вызова команды в контроле cmd_<name>.
type commandEvent struct {
	ID   string `json:"id"`
	TS   int64  `json:"ts"`
	User string `json:"user"`
	Args string `json:"args"`
}

// HandleMessage обрабатывает входящее сообщение Telegram.
func (b *Bot) HandleMessage(_ context.Context, msg telegram.Message) {
	if b.isStopped() {
		return
	}

	user, ok := b.cfg.UserByChatID(msg.ChatID)
	if !ok {
		b.deliver(msg.ChatID, fmt.Sprintf("Нет доступа, ваш chat_id: %d", msg.ChatID))

		return
	}

	name, args, isCommand := parseCommand(msg.Text)
	if !isCommand || name == "start" || name == "help" {
		b.deliver(msg.ChatID, b.helpText(user.Name))

		return
	}

	cmd, found := b.cfg.Command(name)
	if !found || !b.cfg.Allowed(cmd, user.Name) {
		b.deliver(msg.ChatID, "Неизвестная команда.\n\n"+b.helpText(user.Name))

		return
	}

	now := b.options.now()

	if now.Sub(msg.Date) > b.cfg.ReplyTimeout() {
		sentAt := msg.Date.In(b.options.location).Format("15:04")
		b.deliver(msg.ChatID, fmt.Sprintf("Команда /%s устарела (отправлена %s), повторите", name, sentAt))

		return
	}

	if !b.publisher.Connected() {
		b.deliver(msg.ChatID, unavailableText)

		return
	}

	b.publishCall(msg, user.Name, name, args, now)
}

// publishCall публикует вызов в контрол команды и запускает ожидание ответа.
func (b *Bot) publishCall(msg telegram.Message, userName, name, args string, now time.Time) {
	id := strconv.FormatInt(msg.UpdateID, 10)

	payload, errMarshal := json.Marshal(commandEvent{ID: id, TS: now.Unix(), User: userName, Args: args})
	if errMarshal != nil {
		slog.Error("не удалось закодировать вызов команды", "err", errMarshal)

		return
	}

	if !b.track(id, msg.ChatID, name, now) {
		slog.Debug("ядро остановлено, команда не опубликована", "command", name, "id", id)

		return
	}

	errPublish := b.publisher.SetValue(CommandControlPrefix+name, string(payload))
	if errPublish != nil {
		b.untrack(id)
		slog.Error("MQTT: команда не опубликована", "command", name, "err", errPublish)
		b.deliver(msg.ChatID, unavailableText)

		return
	}

	b.startReplyTimer(id)

	slog.Debug("команда опубликована", "command", name, "user", userName, "id", id)
}

// track запоминает вызов (без таймера) и забывает старые вызовы; после Stop возвращает false.
func (b *Bot) track(id string, chatID int64, command string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stopped {
		return false
	}

	for key, old := range b.pending {
		if now.Sub(old.createdAt) > b.options.pendingTTL {
			old.stop()
			delete(b.pending, key)
		}
	}

	b.pending[id] = &pendingCall{chatID: chatID, command: command, createdAt: now}

	return true
}

// startReplyTimer запускает таймер «ответа нет» для опубликованного вызова, если ядро не остановлено.
func (b *Bot) startReplyTimer(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	call, ok := b.pending[id]
	if !ok || b.stopped {
		return
	}

	call.stopTimer = b.options.afterFunc(b.cfg.ReplyTimeout(), func() { b.onReplyTimeout(id) })
}

// untrack забывает вызов и останавливает его таймер.
func (b *Bot) untrack(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	call, ok := b.pending[id]
	if !ok {
		return
	}

	call.stop()
	delete(b.pending, id)
}

// onReplyTimeout сообщает автору, что правило не ответило вовремя.
func (b *Bot) onReplyTimeout(id string) {
	b.mu.Lock()

	call, ok := b.pending[id]
	if !ok || call.answered || b.stopped {
		b.mu.Unlock()

		return
	}

	chatID := call.chatID
	command := call.command

	b.mu.Unlock()

	b.deliver(chatID, fmt.Sprintf("Команда /%s отправлена, ответа от правил нет", command))
}

// parseCommand разбирает "/name@bot args"; имя приводится к нижнему регистру.
func parseCommand(text string) (name, args string, ok bool) {
	trimmed := strings.TrimSpace(text)

	rest, isCommand := strings.CutPrefix(trimmed, "/")
	if !isCommand {
		return "", "", false
	}

	head, tail := rest, ""

	if index := strings.IndexFunc(rest, unicode.IsSpace); index >= 0 {
		head, tail = rest[:index], rest[index:]
	}

	head, _, _ = strings.Cut(head, "@")

	if head == "" {
		return "", "", false
	}

	return strings.ToLower(head), strings.TrimSpace(tail), true
}

// helpText возвращает список команд, доступных пользователю.
func (b *Bot) helpText(userName string) string {
	commands := b.cfg.CommandsFor(userName)
	if len(commands) == 0 {
		return "Для вас пока нет доступных команд."
	}

	lines := make([]string, 0, len(commands)+1)
	lines = append(lines, "Доступные команды:")

	for _, cmd := range commands {
		lines = append(lines, "/"+cmd.Name+" — "+cmd.Description)
	}

	return strings.Join(lines, "\n")
}
