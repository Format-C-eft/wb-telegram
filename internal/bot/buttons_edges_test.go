package bot

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// TestCallbackRejectsInvalidOrigins не публикует команды для чужих или недоступных callback.
func TestCallbackRejectsInvalidOrigins(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		callback telegram.Callback
		answer   string
	}{
		{
			name:     "unknown user",
			callback: telegram.Callback{QueryID: "unknown-user", FromID: 999, ChatID: 999, ChatType: "private", Data: "unknown", Accessible: true},
			answer:   "Кнопка недоступна",
		},
		{
			name:     "group",
			callback: telegram.Callback{QueryID: "group", FromID: 100, ChatID: 100, ChatType: "group", Data: "unknown", Accessible: true},
			answer:   "Кнопка недоступна",
		},
		{
			name:     "inaccessible message",
			callback: telegram.Callback{QueryID: "inaccessible", FromID: 100, ChatID: 100, ChatType: "private", Data: "unknown", Accessible: false},
			answer:   "Кнопка недоступна",
		},
		{
			name:     "unknown token",
			callback: telegram.Callback{QueryID: "unknown-token", FromID: 100, ChatID: 100, ChatType: "private", Data: "unknown", Accessible: true},
			answer:   "Кнопка больше не действует",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			b, deps := newTestBot(t)
			deps.sender.EXPECT().AnswerCallback(gomock.Any(), test.callback.QueryID, test.answer, false).Return(nil)

			b.HandleCallback(context.Background(), test.callback)
			waitForWorkers(t, b)
		})
	}
}

// TestPreparePanelChecksCommandAccess не выдаёт пользователю кнопку на закрытую команду.
func TestPreparePanelChecksCommandAccess(t *testing.T) {
	t.Parallel()

	b, _ := newTestBot(t)

	_, errPanel := b.preparePanel(200, [][]buttonSpec{{{Text: "Состояние", Command: "boiler"}}})
	if !errors.Is(errPanel, ErrBadPayload) {
		t.Fatalf("error = %v, want ErrBadPayload", errPanel)
	}

	if len(b.panels) != 0 {
		t.Fatalf("active panels = %d, want 0", len(b.panels))
	}
}

// TestQueueFullKeepsAcceptedButtonPanel сохраняет панель принятого в очередь сообщения.
func TestQueueFullKeepsAcceptedButtonPanel(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	var token string

	deps.sender.EXPECT().SendDelivery(int64(100), gomock.Any()).DoAndReturn(func(_ int64, delivery telegram.Delivery) error {
		token = delivery.Keyboard[0][0].Data

		return telegram.ErrQueueFull
	})
	deps.publisher.EXPECT().SetValue(gomock.Any(), "send", "Выбрать").Return(nil)
	b.HandleSend([]byte(`{"text":"Выбрать","buttons":[[{"text":"Открыть","command":"gate"}]]}`))

	if len(b.panels) != 1 {
		t.Fatalf("active panels = %d, want 1", len(b.panels))
	}

	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "q", "Принято", false).Return(nil)
	deps.sender.EXPECT().ClearKeyboard(gomock.Any(), int64(100), 7).Return(nil)
	deps.publisher.EXPECT().Connected().Return(true)
	deps.publisher.EXPECT().SetValue(gomock.Any(), "cmd_gate", gomock.Any()).Return(nil)
	b.HandleCallback(context.Background(), telegram.Callback{UpdateID: 11, QueryID: "q", FromID: 100, ChatID: 100, ChatType: "private", MessageID: 7, Data: token, Accessible: true})
	waitForWorkers(t, b)
}

// TestCallbackAckFailureDoesNotReplay проверяет однократность действия при ошибке ACK.
func TestCallbackAckFailureDoesNotReplay(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	panel, errPanel := b.preparePanel(100, [][]buttonSpec{{{Text: "Открыть", Command: "gate"}}})
	if errPanel != nil {
		t.Fatal(errPanel)
	}

	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "first", "Принято", false).Return(errors.New("ack failed"))
	deps.sender.EXPECT().ClearKeyboard(gomock.Any(), int64(100), 7).Return(nil)
	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "retry", "Кнопка больше не действует", false).Return(nil)
	deps.publisher.EXPECT().Connected().Return(true).Times(1)
	deps.publisher.EXPECT().SetValue(gomock.Any(), "cmd_gate", gomock.Any()).Return(nil).Times(1)

	callback := telegram.Callback{UpdateID: 11, FromID: 100, ChatID: 100, ChatType: "private", MessageID: 7, Data: panel.keyboard[0][0].Data, Accessible: true}
	callback.QueryID = "first"
	b.HandleCallback(context.Background(), callback)
	waitForWorkers(t, b)

	callback.QueryID = "retry"
	b.HandleCallback(context.Background(), callback)
	waitForWorkers(t, b)
}

// TestDisconnectedCallbackConsumesWithoutPublishing не повторяет вызов после восстановления MQTT.
func TestDisconnectedCallbackConsumesWithoutPublishing(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	panel, errPanel := b.preparePanel(100, [][]buttonSpec{{{Text: "Открыть", Command: "gate"}}})
	if errPanel != nil {
		t.Fatal(errPanel)
	}

	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "first", "Принято", false).Return(nil)
	deps.publisher.EXPECT().Connected().Return(false).Times(1)
	deps.sender.EXPECT().Send(int64(100), unavailableText).Return(nil)
	deps.sender.EXPECT().ClearKeyboard(gomock.Any(), int64(100), 7).Return(nil)
	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "retry", "Кнопка больше не действует", false).Return(nil)

	callback := telegram.Callback{UpdateID: 11, FromID: 100, ChatID: 100, ChatType: "private", MessageID: 7, Data: panel.keyboard[0][0].Data, Accessible: true}
	callback.QueryID = "first"
	b.HandleCallback(context.Background(), callback)
	waitForWorkers(t, b)

	callback.QueryID = "retry"
	b.HandleCallback(context.Background(), callback)
	waitForWorkers(t, b)
}

// TestRecipientsGetDistinctKeyboardTokens изолирует токены кнопок для каждого адресата.
func TestRecipientsGetDistinctKeyboardTokens(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	var tokens = make(map[int64]string)

	deps.sender.EXPECT().SendDelivery(gomock.Any(), gomock.Any()).DoAndReturn(func(chatID int64, delivery telegram.Delivery) error {
		tokens[chatID] = delivery.Keyboard[0][0].Data

		return nil
	}).Times(2)
	deps.publisher.EXPECT().SetValue(gomock.Any(), "send", "Выбрать").Return(nil)
	b.HandleSend([]byte(`{"text":"Выбрать","to":["Супруга","Ребёнок"],"buttons":[[{"text":"Открыть","command":"gate"}]]}`))

	if tokens[100] == "" || tokens[200] == "" || tokens[100] == tokens[200] {
		t.Fatalf("recipient tokens = %#v, want distinct values", tokens)
	}

	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "wrong", "Кнопка недоступна", false).Return(nil)
	b.HandleCallback(context.Background(), telegram.Callback{QueryID: "wrong", FromID: 200, ChatID: 200, ChatType: "private", Data: tokens[100], Accessible: true})
	waitForWorkers(t, b)
	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "wife", "Принято", false).Return(nil)
	deps.sender.EXPECT().ClearKeyboard(gomock.Any(), int64(100), 7).Return(nil)
	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "child", "Принято", false).Return(nil)
	deps.sender.EXPECT().ClearKeyboard(gomock.Any(), int64(200), 8).Return(nil)
	deps.publisher.EXPECT().Connected().Return(true).Times(2)
	deps.publisher.EXPECT().SetValue(gomock.Any(), "cmd_gate", gomock.Any()).Return(nil).Times(2)
	b.HandleCallback(context.Background(), telegram.Callback{UpdateID: 12, QueryID: "wife", FromID: 100, ChatID: 100, ChatType: "private", MessageID: 7, Data: tokens[100], Accessible: true})
	waitForWorkers(t, b)
	b.HandleCallback(context.Background(), telegram.Callback{UpdateID: 13, QueryID: "child", FromID: 200, ChatID: 200, ChatType: "private", MessageID: 8, Data: tokens[200], Accessible: true})
	waitForWorkers(t, b)
}

// TestButtonPanelPerUserLimit ограничивает число одновременно активных панелей пользователя.
func TestButtonPanelPerUserLimit(t *testing.T) {
	t.Parallel()

	b, _ := newTestBot(t)
	for range maxUserPanels {
		_, errPanel := b.preparePanel(100, [][]buttonSpec{{{Text: "Отмена", Cancel: true}}})
		if errPanel != nil {
			t.Fatal(errPanel)
		}
	}

	_, errPanel := b.preparePanel(100, [][]buttonSpec{{{Text: "Ещё", Cancel: true}}})
	if !errors.Is(errPanel, ErrBadPayload) {
		t.Fatalf("33rd panel error = %v, want ErrBadPayload", errPanel)
	}
}

// TestStoppedBotRejectsButtonPanel не создаёт панель после остановки ядра.
func TestStoppedBotRejectsButtonPanel(t *testing.T) {
	t.Parallel()

	b, _ := newTestBot(t)
	if errStop := b.Stop(); errStop != nil {
		t.Fatal(errStop)
	}

	_, errPanel := b.preparePanel(100, [][]buttonSpec{{{Text: "Отмена", Cancel: true}}})
	if !errors.Is(errPanel, ErrInvalidState) {
		t.Fatalf("panel error = %v, want ErrInvalidState", errPanel)
	}
}
