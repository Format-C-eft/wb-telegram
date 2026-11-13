package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
	"github.com/Format-C-eft/wb-telegram/internal/wbdevice"
)

// unavailableText — ответ, когда команду не удалось передать контроллеру.
const unavailableText = "Контроллер недоступен, команда не выполнена"
const unconfirmedCommandText = "MQTT не подтвердил отправку команды. Проверьте состояние устройства перед повтором."
const commandBusyText = "Очередь команд занята, повторите позже"
const maxQueuedCommands = 16

// commandJob хранит принятую команду до публикации в MQTT.
type commandJob struct {
	updateID int64
	chatID   int64
	user     string
	name     string
	args     string
	received time.Time
	sent     time.Time
	source   commandSource
}

// commandEvent — JSON вызова команды в контроле cmd_<name>.
type commandEvent struct {
	ID                string `json:"id"`
	TS                int64  `json:"ts"`
	User              string `json:"user"`
	Args              string `json:"args"`
	CanReplyEphemeral bool   `json:"can_reply_ephemeral,omitempty"`
}

// HandleMessage обрабатывает входящее сообщение Telegram.
func (b *Bot) HandleMessage(_ context.Context, msg telegram.Message) {
	if !b.beginOperation() {
		return
	}

	defer b.operations.Done()

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

	if name == statusCommand && statusAllowed(user) {
		b.deliver(msg.ChatID, b.statusText())

		return
	}

	cmd, found := b.cfg.Command(name)
	if !found || !b.cfg.Allowed(cmd, user.Name) {
		b.deliver(msg.ChatID, "Неизвестная команда.\n\n"+b.helpText(user.Name))

		return
	}

	now := b.options.now()

	if now.Sub(msg.Date) >= b.cfg.ReplyTimeout() {
		b.reportStaleCommand(msg.ChatID, name, msg.Date)

		return
	}

	if !b.publisher.Connected() {
		b.deliver(msg.ChatID, unavailableText)

		return
	}

	if !b.queueCommand(commandJob{updateID: msg.UpdateID, chatID: msg.ChatID, user: user.Name, name: name, args: args, received: now, sent: msg.Date}) {
		b.deliver(msg.ChatID, commandBusyText)
	}
}

// reportStaleCommand сообщает об истечении срока команды.
func (b *Bot) reportStaleCommand(chatID int64, name string, sent time.Time) {
	b.countStale()

	sentAt := sent.In(b.options.location).Format("15:04")
	b.deliver(chatID, fmt.Sprintf("Команда /%s устарела (отправлена %s), повторите", name, sentAt))
}

// queueCommand принимает команду в ограниченную очередь.
func (b *Bot) queueCommand(job commandJob) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.queueCommandLocked(job)
}

// queueCommandLocked принимает команду под b.mu.
func (b *Bot) queueCommandLocked(job commandJob) bool {
	if b.stopped || len(b.commandJobs) >= maxQueuedCommands {
		return false
	}

	b.commandJobs = append(b.commandJobs, job)

	if b.commandDone == nil {
		b.commandDone = make(chan struct{})
		b.operations.Add(1)

		go b.publishCommands()
	}

	return true
}

// publishCommands публикует команды в порядке приёма.
func (b *Bot) publishCommands() {
	defer b.operations.Done()

	for {
		b.mu.Lock()

		if len(b.commandJobs) == 0 {
			close(b.commandDone)
			b.commandDone = nil
			b.mu.Unlock()

			return
		}

		job := b.commandJobs[0]
		b.commandJobs = slices.Delete(b.commandJobs, 0, 1)

		b.mu.Unlock()

		b.publishCall(job)
	}
}

// publishCall публикует вызов в контрол команды и запускает ожидание ответа.
func (b *Bot) publishCall(job commandJob) {
	now := b.options.now()
	source := job.source

	if b.operationCtx.Err() != nil {
		return
	}

	budget := shortReplyWindow - shortSendBudget
	if source.widget != nil {
		budget = min(budget, b.cfg.ReplyTimeout())
	}

	deadline := job.received.Add(budget)
	if !job.sent.IsZero() {
		deadline = job.sent.Add(b.cfg.ReplyTimeout())
	}

	remaining := deadline.Sub(now)
	if remaining <= 0 {
		b.reportExpiredCommand(job)

		return
	}

	ctx, cancel := context.WithTimeout(b.operationCtx, remaining)
	defer cancel()

	id := strconv.FormatInt(job.updateID, 10)

	payload, errMarshal := json.Marshal(commandEvent{ID: id, TS: now.Unix(), User: job.user, Args: job.args, CanReplyEphemeral: source.callbackID != ""})
	if errMarshal != nil {
		slog.Error("не удалось закодировать вызов команды", "err", errMarshal)

		return
	}

	if !b.track(id, job.chatID, job.name, job.received, source) {
		slog.Debug("команда не поставлена на ожидание ответа", "command", job.name, "id", id)

		return
	}

	errPublish := b.publisher.SetValue(ctx, CommandControlPrefix+job.name, string(payload))
	if b.operationCtx.Err() != nil {
		b.untrack(id)

		return
	}

	if errPublish != nil {
		if errors.Is(errPublish, wbdevice.ErrTimeout) {
			b.reportUnconfirmedCommand(id, job, errPublish)

			return
		}

		b.untrack(id)

		if errors.Is(errPublish, context.DeadlineExceeded) {
			b.reportExpiredCommand(job)

			return
		}

		slog.Error("MQTT: команда не опубликована", "command", job.name, "err", errPublish)

		b.reportCommandFailure(job, unavailableText)

		return
	}

	b.countCall(id, job.name, job.user, now)
	b.startReplyTimer(id)

	slog.Debug("команда опубликована", "command", job.name, "user", job.user, "id", id)
}

// reportExpiredCommand сообщает об истечении срока публикации.
func (b *Bot) reportExpiredCommand(job commandJob) {
	if !job.sent.IsZero() {
		b.reportStaleCommand(job.chatID, job.name, job.sent)

		return
	}

	b.reportCommandFailure(job, "Команда устарела в очереди, повторите")
}

// reportCommandFailure сообщает об ошибке по каналу исходной команды.
func (b *Bot) reportCommandFailure(job commandJob, message string) {
	if job.source.widget != nil {
		b.answerWidgetError(*job.source.widget, job.received.Add(shortReplyWindow), message)

		return
	}

	b.deliver(job.chatID, message)
}

// reportUnconfirmedCommand сообщает об отсутствии подтверждения публикации команды.
func (b *Bot) reportUnconfirmedCommand(id string, job commandJob, errPublish error) {
	b.mu.Lock()

	call := b.pending[id]

	unanswered := call != nil && !call.answered && !b.stopped
	if unanswered {
		call.timeoutNotified = true
	}

	b.mu.Unlock()

	slog.Warn("MQTT: публикация команды не подтверждена", "command", job.name, "err", errPublish)

	if unanswered {
		b.reportCommandFailure(job, unconfirmedCommandText)
	}
}

// track запоминает вызов (без таймера) и забывает старые вызовы; после Stop возвращает false.
func (b *Bot) track(id string, chatID int64, command string, now time.Time, source commandSource) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stopped || b.pendingReply(id) != nil {
		return false
	}

	for key, old := range b.pending {
		if now.Sub(old.createdAt) > b.options.pendingTTL {
			old.stop()
			delete(b.pending, key)
		}
	}

	call := &pendingCall{chatID: chatID, command: command, createdAt: now, source: source}
	switch {
	case source.widget != nil:
		call.replyDeadline = now.Add(min(b.cfg.ReplyTimeout(), shortReplyWindow-shortSendBudget))
	case source.callbackID != "":
		call.replyDeadline = now.Add(shortReplyWindow)
	}

	b.pending[id] = call

	return true
}

// startReplyTimer запускает таймер «ответа нет» для опубликованного вызова, если ядро не остановлено.
func (b *Bot) startReplyTimer(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	call, ok := b.pending[id]
	if !ok || call.answered || b.stopped {
		return
	}

	timeout := b.cfg.ReplyTimeout()

	switch {
	case call.source.widget != nil:
		timeout = call.replyDeadline.Sub(b.options.now())
	case call.source.callbackID != "":
		timeout = max(timeout, call.replyDeadline.Sub(b.options.now()))
	}

	call.stopTimer = b.options.afterFunc(timeout, func() { b.onReplyTimeout(id) })
}

// completeReply завершает ожидание ответа под b.mu.
func (b *Bot) completeReply(id string, call *pendingCall) {
	if call.answered {
		return
	}

	call.answered = true
	call.answeredAt = b.options.now()
	call.stop()
	b.countReply(id, call.answeredAt)
}

// pendingReply возвращает действующий вызов под b.mu.
func (b *Bot) pendingReply(id string) *pendingCall {
	call := b.pending[id]
	if call != nil && b.options.now().Sub(call.createdAt) > b.options.pendingTTL {
		call.stop()
		delete(b.pending, id)

		return nil
	}

	return call
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
	if !b.beginOperation() {
		return
	}
	defer b.operations.Done()

	b.mu.Lock()

	call, ok := b.pending[id]
	if !ok || call.answered || call.timeoutNotified || b.stopped {
		b.mu.Unlock()

		return
	}

	call.timeoutNotified = true
	chatID := call.chatID
	command := call.command
	widget := call.source.widget
	deadline := call.createdAt.Add(shortReplyWindow)

	b.mu.Unlock()

	b.countUnanswered()

	if widget != nil {
		b.answerWidgetError(*widget, deadline, "Правило не ответило вовремя")

		return
	}

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
	user, _ := b.cfg.UserByName(userName)

	if len(commands) == 0 && !statusAllowed(user) {
		return "Для вас пока нет доступных команд."
	}

	lines := make([]string, 0, len(commands)+2)
	lines = append(lines, "Доступные команды:")

	for _, cmd := range commands {
		lines = append(lines, "/"+cmd.Name+" — "+cmd.Description)
	}

	if statusAllowed(user) {
		lines = append(lines, "/"+statusCommand+" — "+statusDescription)
	}

	return strings.Join(lines, "\n")
}
