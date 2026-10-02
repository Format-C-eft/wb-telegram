package bot

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// message создаёт входящее сообщение от chatID с датой testNow.
func message(updateID, chatID int64, text string) telegram.Message {
	return telegram.Message{UpdateID: updateID, ChatID: chatID, Text: text, Date: testNow}
}

// TestParseCommand проверяет разбор текста команды.
func TestParseCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		wantName string
		wantArgs string
		wantOK   bool
	}{
		{name: "plain", text: "/gate", wantName: "gate", wantOK: true},
		{name: "args", text: "/gate 22", wantName: "gate", wantArgs: "22", wantOK: true},
		{name: "upper case", text: "/Gate", wantName: "gate", wantOK: true},
		{name: "bot mention", text: "/gate@MyHomeBot 22", wantName: "gate", wantArgs: "22", wantOK: true},
		{name: "bot mention extra spaces", text: "/gate@MyHomeBot  22 градуса ", wantName: "gate", wantArgs: "22 градуса", wantOK: true},
		{name: "trailing spaces", text: "/gate   ", wantName: "gate", wantOK: true},
		{name: "surrounding spaces", text: "  /gate   ", wantName: "gate", wantOK: true},
		{name: "newline before args", text: "/gate\n22", wantName: "gate", wantArgs: "22", wantOK: true},
		{name: "slash only", text: "/", wantOK: false},
		{name: "mention only", text: "/@bot", wantOK: false},
		{name: "plain text", text: "открой ворота", wantOK: false},
		{name: "empty", text: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			name, args, ok := parseCommand(tt.text)
			if name != tt.wantName || args != tt.wantArgs || ok != tt.wantOK {
				t.Fatalf("parseCommand(%q) = %q, %q, %v; want %q, %q, %v", tt.text, name, args, ok, tt.wantName, tt.wantArgs, tt.wantOK)
			}
		})
	}
}

// TestHandleMessageNoAccess проверяет ответ незнакомому chat_id.
func TestHandleMessageNoAccess(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	deps.sender.EXPECT().Send(int64(999), "Нет доступа, ваш chat_id: 999").Return(nil)

	b.HandleMessage(context.Background(), message(1, 999, "/gate"))
}

// TestHandleMessageHelp проверяет справку только по доступным командам.
func TestHandleMessageHelp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		chatID int64
		text   string
		want   string
	}{
		{name: "help child", chatID: 200, text: "/help", want: "Доступные команды:\n/gate — Открыть ворота"},
		{name: "start wife", chatID: 100, text: "/start", want: "Доступные команды:\n/gate — Открыть ворота\n/boiler — Состояние котла"},
		{name: "help with mention", chatID: 200, text: "/Help@MyHomeBot", want: "Доступные команды:\n/gate — Открыть ворота"},
		{name: "plain text", chatID: 200, text: "привет", want: "Доступные команды:\n/gate — Открыть ворота"},
		{name: "slash only", chatID: 200, text: "/", want: "Доступные команды:\n/gate — Открыть ворота"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, deps := newTestBot(t)
			deps.sender.EXPECT().Send(tt.chatID, tt.want).Return(nil)

			b.HandleMessage(context.Background(), message(1, tt.chatID, tt.text))
		})
	}
}

// TestHandleMessagePublishes проверяет публикацию JSON вызова в контрол команды.
func TestHandleMessagePublishes(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	var published string

	deps.publisher.EXPECT().Connected().Return(true)
	deps.publisher.EXPECT().SetValue("cmd_gate", gomock.Any()).DoAndReturn(func(_, value string) error {
		published = value

		return nil
	})

	b.HandleMessage(context.Background(), message(42, 100, "/gate@MyHomeBot 22"))

	var got commandEvent

	errUnmarshal := json.Unmarshal([]byte(published), &got)
	if errUnmarshal != nil {
		t.Fatalf("payload %q: %v", published, errUnmarshal)
	}

	want := commandEvent{ID: "42", TS: testNow.Unix(), User: "Супруга", Args: "22"}
	if got != want {
		t.Fatalf("payload = %+v, want %+v", got, want)
	}

	if deps.timers.count() != 1 {
		t.Fatalf("reply timer count = %d, want 1", deps.timers.count())
	}

	if got := deps.timers.duration(0); got != 10*time.Second {
		t.Fatalf("reply timer = %v, want 10s", got)
	}
}

// TestHandleMessageStaleBoundary проверяет границу устаревания: ровно reply_timeout — ещё не устарела.
func TestHandleMessageStaleBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		age   time.Duration
		stale bool
	}{
		{name: "fresh", age: 0},
		{name: "exactly reply timeout", age: 10 * time.Second},
		{name: "reply timeout plus second", age: 11 * time.Second, stale: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, deps := newTestBot(t)
			msg := telegram.Message{UpdateID: 1, ChatID: 100, Text: "/gate", Date: testNow.Add(-tt.age)}

			if tt.stale {
				deps.sender.EXPECT().Send(int64(100), "Команда /gate устарела (отправлена 14:04), повторите").Return(nil)
			} else {
				deps.publisher.EXPECT().Connected().Return(true)
				deps.publisher.EXPECT().SetValue("cmd_gate", gomock.Any()).Return(nil)
			}

			b.HandleMessage(context.Background(), msg)
		})
	}
}

// TestHandleMessageRejections проверяет отказы: нет доступа к команде, устарела, нет MQTT.
func TestHandleMessageRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		msg     telegram.Message
		prepare func(d testDeps)
		want    string
	}{
		{
			name: "command not allowed",
			msg:  message(1, 200, "/boiler"),
			want: "Неизвестная команда.\n\nДоступные команды:\n/gate — Открыть ворота",
		},
		{
			name: "unknown command",
			msg:  message(1, 200, "/nope"),
			want: "Неизвестная команда.\n\nДоступные команды:\n/gate — Открыть ворота",
		},
		{
			name: "stale",
			msg:  telegram.Message{UpdateID: 1, ChatID: 100, Text: "/gate", Date: testNow.Add(-3 * time.Minute)},
			want: "Команда /gate устарела (отправлена 14:02), повторите",
		},
		{
			name:    "mqtt disconnected",
			msg:     message(1, 100, "/gate"),
			prepare: func(d testDeps) { d.publisher.EXPECT().Connected().Return(false) },
			want:    "Контроллер недоступен, команда не выполнена",
		},
		{
			name: "publish failed",
			msg:  message(1, 100, "/gate"),
			prepare: func(d testDeps) {
				d.publisher.EXPECT().Connected().Return(true)
				d.publisher.EXPECT().SetValue("cmd_gate", gomock.Any()).Return(errors.New("timeout"))
			},
			want: "Контроллер недоступен, команда не выполнена",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, deps := newTestBot(t)

			if tt.prepare != nil {
				tt.prepare(deps)
			}

			deps.sender.EXPECT().Send(tt.msg.ChatID, tt.want).Return(nil)

			b.HandleMessage(context.Background(), tt.msg)

			b.mu.Lock()
			pending := len(b.pending)
			b.mu.Unlock()

			if pending != 0 {
				t.Fatalf("pending calls = %d, want 0", pending)
			}

			if deps.timers.count() != 0 {
				t.Fatalf("reply timer count = %d, want 0", deps.timers.count())
			}
		})
	}
}

// TestHandleMessageQueueFull проверяет, что переполнение очереди при ответе не ломает обработку.
func TestHandleMessageQueueFull(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	deps.sender.EXPECT().Send(int64(999), gomock.Any()).Return(telegram.ErrQueueFull)

	b.HandleMessage(context.Background(), message(1, 999, "/gate"))
}

// TestReplyTimeout проверяет сообщение «ответа от правил нет» по таймеру.
func TestReplyTimeout(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	deps.publisher.EXPECT().Connected().Return(true)
	deps.publisher.EXPECT().SetValue("cmd_gate", gomock.Any()).Return(nil)
	deps.sender.EXPECT().Send(int64(100), "Команда /gate отправлена, ответа от правил нет").Return(nil)

	b.HandleMessage(context.Background(), message(42, 100, "/gate"))
	deps.timers.fire(0)
}

// TestStopStopsReplyTimers проверяет, что после Stop таймер ожидания ответа не срабатывает.
func TestStopStopsReplyTimers(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	deps.publisher.EXPECT().Connected().Return(true)
	deps.publisher.EXPECT().SetValue("cmd_gate", gomock.Any()).Return(nil)

	b.HandleMessage(context.Background(), message(42, 100, "/gate"))

	errStop := b.Stop()
	if errStop != nil {
		t.Fatalf("Stop: %v", errStop)
	}

	deps.timers.fire(0)
}

// TestHandleMessageAfterStop проверяет, что после Stop команды не принимаются и таймеры не создаются.
func TestHandleMessageAfterStop(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)

	errStop := b.Stop()
	if errStop != nil {
		t.Fatalf("Stop: %v", errStop)
	}

	b.HandleMessage(context.Background(), message(42, 100, "/gate"))

	if deps.timers.count() != 0 {
		t.Fatalf("reply timer count = %d, want 0", deps.timers.count())
	}
}

// TestRunGuards проверяет отказ на повторный Run и на Run после Stop.
func TestRunGuards(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		stopFirst bool
	}{
		{name: "run twice"},
		{name: "run after stop", stopFirst: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, deps := newTestBot(t)
			deps.sender.EXPECT().SetCommands(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

			if tt.stopFirst {
				errStop := b.Stop()
				if errStop != nil {
					t.Fatalf("Stop: %v", errStop)
				}
			} else {
				errFirst := b.Run(context.Background())
				if errFirst != nil {
					t.Fatalf("first Run: %v", errFirst)
				}
			}

			errRun := b.Run(context.Background())
			if !errors.Is(errRun, ErrInvalidState) {
				t.Fatalf("Run error = %v, want ErrInvalidState", errRun)
			}

			errStop := b.Stop()
			if errStop != nil {
				t.Fatalf("Stop: %v", errStop)
			}
		})
	}
}

// TestRunSyncsCommands проверяет установку меню каждому пользователю с повторами после временных ошибок и 429.
func TestRunSyncsCommands(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		delays []time.Duration
	)

	b, deps := newTestBot(t, withSleep(func(ctx context.Context, d time.Duration) bool {
		mu.Lock()

		delays = append(delays, d)

		mu.Unlock()

		return ctx.Err() == nil
	}))

	all := []telegram.Command{{Name: "gate", Description: "Открыть ворота"}, {Name: "boiler", Description: "Состояние котла"}}
	child := []telegram.Command{{Name: "gate", Description: "Открыть ворота"}}

	rateLimited := *telegram.ErrRateLimited
	rateLimited.RetryAfter = 5 * time.Second

	gomock.InOrder(
		deps.sender.EXPECT().SetCommands(gomock.Any(), int64(100), all).Return(telegram.ErrTransient),
		deps.sender.EXPECT().SetCommands(gomock.Any(), int64(100), all).Return(&rateLimited),
		deps.sender.EXPECT().SetCommands(gomock.Any(), int64(100), all).Return(nil),
		deps.sender.EXPECT().SetCommands(gomock.Any(), int64(200), child).Return(nil),
	)

	errRun := b.Run(context.Background())
	if errRun != nil {
		t.Fatalf("Run: %v", errRun)
	}

	<-b.done

	errStop := b.Stop()
	if errStop != nil {
		t.Fatalf("Stop: %v", errStop)
	}

	mu.Lock()
	defer mu.Unlock()

	// Вторая пауза — retry_after из ответа 429 (5 с), а не экспоненциальные 2 с.
	want := []time.Duration{time.Second, 5 * time.Second}
	if !slices.Equal(delays, want) {
		t.Fatalf("retry delays = %v, want %v", delays, want)
	}
}

// TestRunSkipsPermanentErrors проверяет, что окончательные отказы не повторяются, а меню ставится остальным.
func TestRunSkipsPermanentErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{name: "unauthorized", err: telegram.ErrUnauthorized},
		{name: "rejected", err: telegram.ErrRejected},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, deps := newTestBot(t)

			gomock.InOrder(
				deps.sender.EXPECT().SetCommands(gomock.Any(), int64(100), gomock.Any()).Return(tt.err),
				deps.sender.EXPECT().SetCommands(gomock.Any(), int64(200), gomock.Any()).Return(nil),
			)

			errRun := b.Run(context.Background())
			if errRun != nil {
				t.Fatalf("Run: %v", errRun)
			}

			<-b.done

			errStop := b.Stop()
			if errStop != nil {
				t.Fatalf("Stop: %v", errStop)
			}
		})
	}
}

// TestStopCancelsRun проверяет, что Stop прерывает повторы установки меню.
func TestStopCancelsRun(t *testing.T) {
	t.Parallel()

	sleeping := make(chan struct{})

	var once sync.Once

	b, deps := newTestBot(t, withSleep(func(ctx context.Context, _ time.Duration) bool {
		once.Do(func() { close(sleeping) })
		<-ctx.Done()

		return false
	}))

	deps.sender.EXPECT().SetCommands(gomock.Any(), int64(100), gomock.Any()).Return(telegram.ErrTransient)

	errRun := b.Run(context.Background())
	if errRun != nil {
		t.Fatalf("Run: %v", errRun)
	}

	<-sleeping

	errStop := b.Stop()
	if errStop != nil {
		t.Fatalf("Stop: %v", errStop)
	}
}

// TestNewRejectsNil проверяет отказ на nil-зависимостях.
func TestNewRejectsNil(t *testing.T) {
	t.Parallel()

	_, errNew := New(nil, nil, nil)
	if !errors.Is(errNew, ErrInvalidArgument) {
		t.Fatalf("New error = %v, want ErrInvalidArgument", errNew)
	}
}

// TestHelpTextNoCommands проверяет справку для пользователя без команд.
func TestHelpTextNoCommands(t *testing.T) {
	t.Parallel()

	b, _ := newTestBot(t)
	b.cfg.Commands[0].Users = []string{"Супруга"}

	if got := b.helpText("Ребёнок"); !strings.Contains(got, "Для вас пока нет доступных команд.") {
		t.Fatalf("helpText = %q", got)
	}
}
