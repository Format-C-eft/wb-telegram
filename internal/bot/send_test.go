package bot

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// callGate публикует вызов /gate от chatID с updateID.
func callGate(b *Bot, deps testDeps, updateID, chatID int64) {
	deps.publisher.EXPECT().Connected().Return(true)
	deps.publisher.EXPECT().SetValue("cmd_gate", gomock.Any()).Return(nil)

	b.HandleMessage(context.Background(), message(updateID, chatID, "/gate"))
}

// TestHandleSendRecipients проверяет выбор получателей по виду payload.
func TestHandleSendRecipients(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload string
		text    string
		chats   []int64
	}{
		{name: "plain text to subscribers", payload: "Котёл: ошибка E04", text: "Котёл: ошибка E04", chats: []int64{100}},
		{name: "plain text trimmed", payload: "  Котёл  \n", text: "Котёл", chats: []int64{100}},
		{name: "json text to subscribers", payload: `{"text":"Свет выключен","id":"wbr-1"}`, text: "Свет выключен", chats: []int64{100}},
		{name: "to named users", payload: `{"text":"Курьер у ворот","to":["Ребёнок","Гость"]}`, text: "Курьер у ворот", chats: []int64{200}},
		{name: "to both deduplicated", payload: `{"text":"Ужин","to":["Ребёнок","Супруга","Ребёнок"]}`, text: "Ужин", chats: []int64{200, 100}},
		{name: "to unknown only", payload: `{"text":"Никому","to":["Гость"]}`, text: "Никому"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, deps := newTestBot(t)

			calls := make([]any, 0, len(tt.chats)+1)

			for _, chat := range tt.chats {
				calls = append(calls, deps.sender.EXPECT().Send(chat, tt.text).Return(nil))
			}

			calls = append(calls, deps.publisher.EXPECT().SetValue("send", tt.text).Return(nil))

			gomock.InOrder(calls...)

			b.HandleSend([]byte(tt.payload))
		})
	}
}

// TestHandleSendIgnoresBadPayload проверяет, что пустые и битые сообщения не отправляются.
func TestHandleSendIgnoresBadPayload(t *testing.T) {
	t.Parallel()

	for _, payload := range []string{"", "   ", `{"text":""}`, `{"text":"  "}`, `{"text": "x"`, `{"text":5}`} {
		t.Run(payload, func(t *testing.T) {
			t.Parallel()

			b, _ := newTestBot(t)

			b.HandleSend([]byte(payload))
		})
	}
}

// TestHandleSendRoutesConcurrentCallers проверяет, что ответ уходит автору нужного вызова.
func TestHandleSendRoutesConcurrentCallers(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	callGate(b, deps, 1, 100)
	callGate(b, deps, 2, 200)

	deps.sender.EXPECT().Send(int64(200), "Ворота открываются").Return(nil)
	deps.publisher.EXPECT().SetValue("send", "Ворота открываются").Return(nil)

	b.HandleSend([]byte(`{"text":"Ворота открываются","reply_to":"2","id":"wbr-5"}`))

	deps.sender.EXPECT().Send(int64(100), "Уже открыты").Return(nil)
	deps.publisher.EXPECT().SetValue("send", "Уже открыты").Return(nil)

	b.HandleSend([]byte(`{"text":"Уже открыты","reply_to":"1"}`))

	// Оба вызова получили ответ: «ответа нет» не отправляется.
	deps.timers.fire(0)
	deps.timers.fire(1)
}

// TestHandleSendReplyToWinsOverTo проверяет, что reply_to важнее списка to.
func TestHandleSendReplyToWinsOverTo(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	callGate(b, deps, 3, 200)

	deps.sender.EXPECT().Send(int64(200), "Готово").Return(nil)
	deps.publisher.EXPECT().SetValue("send", "Готово").Return(nil)

	b.HandleSend([]byte(`{"text":"Готово","reply_to":"3","to":["Супруга"]}`))
}

// TestHandleSendLateAndRepeatedReplies проверяет доставку повторных ответов и ответа после таймаута.
func TestHandleSendLateAndRepeatedReplies(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	callGate(b, deps, 7, 100)

	deps.sender.EXPECT().Send(int64(100), "Команда /gate отправлена, ответа от правил нет").Return(nil)
	deps.timers.fire(0)

	for _, text := range []string{"Открываю", "Открыто"} {
		deps.sender.EXPECT().Send(int64(100), text).Return(nil)
		deps.publisher.EXPECT().SetValue("send", text).Return(nil)

		b.HandleSend([]byte(`{"text":"` + text + `","reply_to":"7"}`))
	}
}

// TestHandleSendUnknownReply проверяет, что ответ на неизвестный вызов никому не отправляется.
func TestHandleSendUnknownReply(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	deps.publisher.EXPECT().SetValue("send", "x").Return(nil)

	b.HandleSend([]byte(`{"text":"x","reply_to":"404"}`))
}

// TestHandleSendPublishFailure проверяет, что ошибка публикации в send не мешает доставке.
func TestHandleSendPublishFailure(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	deps.sender.EXPECT().Send(int64(100), "x").Return(nil)
	deps.publisher.EXPECT().SetValue("send", "x").Return(errors.New("no connection"))

	b.HandleSend([]byte("x"))
}

// TestHandleSendAfterStop проверяет, что после Stop сообщения правил не отправляются.
func TestHandleSendAfterStop(t *testing.T) {
	t.Parallel()

	b, _ := newTestBot(t)

	errStop := b.Stop()
	if errStop != nil {
		t.Fatalf("Stop: %v", errStop)
	}

	b.HandleSend([]byte("x"))
}

// TestReportDelivery проверяет публикацию флага w только при смене состояния.
func TestReportDelivery(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	gomock.InOrder(
		deps.publisher.EXPECT().SetError("send", "w").Return(nil),
		deps.publisher.EXPECT().SetError("send", "").Return(nil),
	)

	b.ReportDelivery(nil)
	b.ReportDelivery(telegram.ErrRejected)
	b.ReportDelivery(errors.New("again"))
	b.ReportDelivery(nil)
	b.ReportDelivery(nil)
}

// TestReportDeliveryConcurrent проверяет, что при одновременных отчётах флаги публикуются строго
// по очереди смены состояния: w и "" чередуются, последний опубликованный флаг совпадает с итогом.
func TestReportDeliveryConcurrent(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	var (
		mu        sync.Mutex
		published []string
	)

	deps.publisher.EXPECT().SetError("send", gomock.Any()).DoAndReturn(func(_, flags string) error {
		mu.Lock()

		published = append(published, flags)

		mu.Unlock()

		return nil
	}).AnyTimes()

	var wg sync.WaitGroup

	for worker := range 8 {
		wg.Go(func() {
			for i := range 200 {
				if (worker+i)%2 == 0 {
					b.ReportDelivery(telegram.ErrTransient)
				} else {
					b.ReportDelivery(nil)
				}
			}
		})
	}

	wg.Wait()

	b.ReportDelivery(nil)

	mu.Lock()
	defer mu.Unlock()

	want := "w"

	for i, flags := range published {
		if flags != want {
			t.Fatalf("published[%d] = %q, want %q: flags must alternate", i, flags, want)
		}

		if want == "w" {
			want = ""
		} else {
			want = "w"
		}
	}

	if len(published) > 0 && published[len(published)-1] != "" {
		t.Fatalf("last published flag = %q, want empty", published[len(published)-1])
	}
}

// TestHandleSendQueueFull проверяет флаг w при переполнении очереди: о переполнении сообщает клиент
// через onDelivery (подключён к ReportDelivery), ядро не дублирует отчёт.
func TestHandleSendQueueFull(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	deps.sender.EXPECT().Send(int64(100), "x").DoAndReturn(func(int64, string) error {
		b.ReportDelivery(telegram.ErrQueueFull)

		return telegram.ErrQueueFull
	})
	deps.publisher.EXPECT().SetError("send", "w").Return(nil)
	deps.publisher.EXPECT().SetValue("send", "x").Return(nil)

	b.HandleSend([]byte("x"))
}
