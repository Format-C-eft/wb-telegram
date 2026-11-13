package bot

import (
	"context"
	"log/slog"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// HandleCallback проверяет и исполняет одноразовый выбор пользователя.
func (b *Bot) HandleCallback(_ context.Context, callback telegram.Callback) {
	if !b.beginOperation() {
		return
	}
	defer b.operations.Done()

	if !callback.Accessible || callback.ChatType != "private" || callback.ChatID <= 0 || callback.FromID != callback.ChatID {
		b.answerCallback(callback, "Кнопка недоступна")

		return
	}

	user, known := b.cfg.UserByChatID(callback.ChatID)
	if !known {
		b.answerCallback(callback, "Кнопка недоступна")

		return
	}

	now := b.options.now()
	if b.acceptButton(callback, user.Name, now) {
		b.deliver(callback.ChatID, unavailableText)
	}
}

// acceptButton обрабатывает выбор и сообщает, недоступен ли контроллер.
func (b *Bot) acceptButton(callback telegram.Callback, userName string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.prunePanelsLocked(now)

	status, consumed, offline := b.applyButton(callback, userName, now)
	if callback.QueryID != "" || consumed {
		b.queueShortReplyLocked(b.callbackReply(callback, status, consumed, now))
	}

	return offline
}

// applyButton принимает выбор и обновляет панель под b.mu.
func (b *Bot) applyButton(callback telegram.Callback, userName string, now time.Time) (status string, consumed, offline bool) {
	action, found := b.buttons[callback.Data]
	if !found || b.stopped {
		return "Кнопка больше не действует", false, false
	}

	if action.panel.chatID != callback.ChatID {
		return "Кнопка недоступна", false, false
	}

	if action.spec.Cancel {
		b.removePanelLocked(action.panel)

		return "Отменено", true, false
	}

	command, known := b.cfg.Command(action.spec.Command)
	if !known || !b.cfg.Allowed(command, userName) {
		return "Кнопка недоступна", false, false
	}

	connected := b.publisher.Connected()
	if connected && !b.queueCommandLocked(commandJob{
		updateID: callback.UpdateID, chatID: callback.ChatID, user: userName, name: action.spec.Command, args: action.spec.Args,
		received: now, source: commandSource{callbackID: callback.QueryID},
	}) {
		return commandBusyText, false, false
	}

	b.removePanelLocked(action.panel)

	return "Принято", true, !connected
}

// answerCallback отвечает на нажатие вне очереди сообщений.
func (b *Bot) answerCallback(callback telegram.Callback, text string) {
	if callback.QueryID == "" {
		return
	}

	b.queueShortReply(b.callbackReply(callback, text, false, b.options.now()))
}

// callbackReply создаёт подтверждение нажатия и снятие кнопок.
func (b *Bot) callbackReply(callback telegram.Callback, text string, clear bool, received time.Time) shortReply {
	return shortReply{deadline: received.Add(shortReplyWindow), callback: callback.QueryID, send: func(ctx context.Context) {
		if callback.QueryID != "" {
			errAnswer := b.sender.AnswerCallback(ctx, callback.QueryID, text, false)
			if errAnswer != nil {
				slog.Debug("Telegram: ответ на нажатие не отправлен", "err", errAnswer)
			}
		}

		if clear && ctx.Err() == nil {
			b.clearKeyboard(ctx, callback)
		}
	}}
}

// clearKeyboard снимает отработавшую клавиатуру без повторения действия.
func (b *Bot) clearKeyboard(ctx context.Context, callback telegram.Callback) {
	errClear := b.sender.ClearKeyboard(ctx, callback.ChatID, callback.MessageID)
	if errClear != nil {
		slog.Debug("Telegram: клавиатура не снята", "err", errClear)
	}
}
