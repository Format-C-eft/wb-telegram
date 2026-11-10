package bot

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// TestFormattedReply сохраняет формат и отступы ответа правила.
func TestFormattedReply(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	deps.sender.EXPECT().SendDelivery(int64(100), gomock.Any()).DoAndReturn(func(_ int64, d telegram.Delivery) error {
		if d.Kind != telegram.DeliveryText || d.ParseMode != telegram.ParseModeHTML || d.Text != "  <b>Котёл</b>  " {
			t.Errorf("delivery = %+v", d)
		}

		return nil
	})
	deps.publisher.EXPECT().SetValue("send", "  <b>Котёл</b>  ").Return(nil)
	b.HandleSend([]byte(`{"text":"  <b>Котёл</b>  ","parse_mode":"HTML"}`))
}

// TestButtonPanelConsumesOnce не допускает повторное выполнение выбора.
func TestButtonPanelConsumesOnce(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	var data string

	deps.sender.EXPECT().SendDelivery(int64(100), gomock.Any()).DoAndReturn(func(_ int64, d telegram.Delivery) error {
		data = d.Keyboard[0][0].Data

		return nil
	})
	deps.publisher.EXPECT().SetValue("send", "Открыть?").Return(nil)
	b.HandleSend([]byte(`{"text":"Открыть?","buttons":[[{"text":"Да","command":"gate","args":"open"},{"text":"Нет","cancel":true}]]}`))

	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "q", "Принято", false).Return(nil)
	deps.sender.EXPECT().ClearKeyboard(gomock.Any(), int64(100), 7).Return(nil)
	deps.publisher.EXPECT().Connected().Return(true)
	deps.publisher.EXPECT().SetValue("cmd_gate", gomock.Any()).DoAndReturn(func(_ string, raw string) error {
		var call commandEvent

		errJSON := json.Unmarshal([]byte(raw), &call)
		if errJSON != nil {
			t.Fatal(errJSON)
		}

		if call.ID != "20" || call.Args != "open" || call.User != "Супруга" {
			t.Errorf("call = %+v", call)
		}

		return nil
	})

	cb := telegram.Callback{UpdateID: 20, QueryID: "q", FromID: 100, ChatID: 100, ChatType: "private", MessageID: 7, Data: data, Accessible: true}
	b.HandleCallback(context.Background(), cb)

	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "q", "Кнопка больше не действует", false).Return(nil)
	b.HandleCallback(context.Background(), cb)
}

// TestCallbackRejectsForeignUser не разрешает чужому пользователю потребить панель.
func TestCallbackRejectsForeignUser(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	var data string

	deps.sender.EXPECT().SendDelivery(int64(100), gomock.Any()).DoAndReturn(func(_ int64, d telegram.Delivery) error {
		data = d.Keyboard[0][0].Data

		return nil
	})
	deps.publisher.EXPECT().SetValue("send", "Отмена?").Return(nil)
	b.HandleSend([]byte(`{"text":"Отмена?","buttons":[[{"text":"Нет","cancel":true}]]}`))
	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "q", "Кнопка недоступна", false).Return(nil)
	b.HandleCallback(context.Background(), telegram.Callback{QueryID: "q", FromID: 200, ChatID: 100, ChatType: "private", MessageID: 7, Data: data, Accessible: true})
	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "q", "Отменено", false).Return(nil)
	deps.sender.EXPECT().ClearKeyboard(gomock.Any(), int64(100), 7).Return(nil)
	b.HandleCallback(context.Background(), telegram.Callback{QueryID: "q", FromID: 100, ChatID: 100, ChatType: "private", MessageID: 7, Data: data, Accessible: true})
}

// TestPanelExpiryAndClosedQueue очищает просроченные и недоставленные панели.
func TestPanelExpiryAndClosedQueue(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	deps.sender.EXPECT().SendDelivery(int64(100), gomock.Any()).Return(telegram.ErrClosed)
	deps.publisher.EXPECT().SetValue("send", "Выбор").Return(nil)
	b.HandleSend([]byte(`{"text":"Выбор","buttons":[[{"text":"Нет","cancel":true}]]}`))

	if len(b.panels) != 0 {
		t.Fatal("closed queue retained a panel")
	}

	panel, errPanel := b.preparePanel(100, [][]buttonSpec{{{Text: "Нет", Cancel: true}}})
	if errPanel != nil {
		t.Fatal(errPanel)
	}

	b.options.now = func() time.Time { return testNow.Add(6 * time.Minute) }

	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "q", "Кнопка больше не действует", false).Return(nil)
	b.HandleCallback(context.Background(), telegram.Callback{QueryID: "q", FromID: 100, ChatID: 100, ChatType: "private", Data: panel.keyboard[0][0].Data, Accessible: true})
}

// TestConcurrentCallbacks публикует только один вызов при одновременном нажатии.
func TestConcurrentCallbacks(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	panel, errPanel := b.preparePanel(100, [][]buttonSpec{{{Text: "Да", Command: "gate"}}})
	if errPanel != nil {
		t.Fatal(errPanel)
	}

	deps.sender.EXPECT().AnswerCallback(gomock.Any(), gomock.Any(), gomock.Any(), false).Return(nil).Times(2)
	deps.sender.EXPECT().ClearKeyboard(gomock.Any(), int64(100), 7).Return(nil)
	deps.publisher.EXPECT().Connected().Return(true)
	deps.publisher.EXPECT().SetValue("cmd_gate", gomock.Any()).Return(nil)

	var workers sync.WaitGroup
	workers.Add(2)

	for i := range 2 {
		go func() {
			defer workers.Done()

			b.HandleCallback(context.Background(), telegram.Callback{UpdateID: int64(i + 10), QueryID: "q", FromID: 100, ChatID: 100, ChatType: "private", MessageID: 7, Data: panel.keyboard[0][0].Data, Accessible: true})
		}()
	}

	workers.Wait()
}

// TestInvalidInteractionPayload не принимает противоречивые сообщения.
func TestInvalidInteractionPayload(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{`{"text":"x","parse_mode":"Markdown"}`, `{"text":"x","buttons":[]}`, `{"text":"x","buttons":[[{"text":"X","command":"gate","cancel":true}]]}`, `{"chart":{},"text":"x"}`} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			_, errParse := parseSendRequest([]byte(raw))
			if !errors.Is(errParse, ErrBadPayload) {
				t.Fatalf("error = %v", errParse)
			}
		})
	}
}
