package bot

import (
	"context"
	"log/slog"

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

	action, status := b.consumeButton(callback, user.Name)
	if status != "" {
		b.answerCallback(callback, status)

		return
	}

	if action.Cancel {
		b.answerCallback(callback, "Отменено")
		b.clearKeyboard(callback)

		return
	}

	b.answerCallback(callback, "Принято")

	if !b.publisher.Connected() {
		b.deliver(callback.ChatID, unavailableText)
		b.clearKeyboard(callback)

		return
	}

	if b.operationCtx.Err() == nil {
		now := b.options.now()
		b.publishCall(telegram.Message{UpdateID: callback.UpdateID, ChatID: callback.ChatID, Date: now}, user.Name, action.Command, action.Args, now)
	}

	b.clearKeyboard(callback)
}

// consumeButton атомарно потребляет всю панель допустимого адресата.
func (b *Bot) consumeButton(callback telegram.Callback, userName string) (buttonSpec, string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.prunePanelsLocked(b.options.now())

	action, found := b.buttons[callback.Data]
	if !found || b.stopped {
		return buttonSpec{}, "Кнопка больше не действует"
	}

	if action.panel.chatID != callback.ChatID {
		return buttonSpec{}, "Кнопка недоступна"
	}

	if !action.spec.Cancel {
		command, known := b.cfg.Command(action.spec.Command)
		if !known || !b.cfg.Allowed(command, userName) {
			return buttonSpec{}, "Кнопка недоступна"
		}
	}

	b.removePanelLocked(action.panel)

	return action.spec, ""
}

// answerCallback отвечает на нажатие вне очереди сообщений.
func (b *Bot) answerCallback(callback telegram.Callback, text string) {
	if callback.QueryID == "" {
		return
	}

	errAnswer := b.sender.AnswerCallback(b.operationCtx, callback.QueryID, text, false)
	if errAnswer != nil {
		slog.Debug("Telegram: ответ на нажатие не отправлен", "err", errAnswer)
	}
}

// clearKeyboard снимает отработавшую клавиатуру без повторения действия.
func (b *Bot) clearKeyboard(callback telegram.Callback) {
	if b.operationCtx.Err() != nil {
		return
	}

	errClear := b.sender.ClearKeyboard(b.operationCtx, callback.ChatID, callback.MessageID)
	if errClear != nil {
		slog.Debug("Telegram: клавиатура не снята", "err", errClear)
	}
}
