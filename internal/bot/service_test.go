package bot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// movableClock — часы, которые тест переводит вручную.
type movableClock struct {
	mu  sync.Mutex
	now time.Time
}

// get возвращает текущее время часов.
func (c *movableClock) get() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

// advance переводит часы вперёд.
func (c *movableClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// TestReportTelegram проверяет сообщение о восстановлении связи с Telegram: одно на обрыв,
// с длительностью, временем начала и понятной причиной.
func TestReportTelegram(t *testing.T) {
	t.Parallel()

	clock := &movableClock{now: testNow}
	b, deps := newTestBot(t, withClock(clock.get))

	gomock.InOrder(
		deps.sender.EXPECT().Send(int64(100),
			"⚠️ Нет связи с Telegram уже 30 с (с 14:05:00), причина: соединение оборвалось").Return(nil),
		deps.sender.EXPECT().Send(int64(100),
			"✅ Связь с Telegram восстановлена. Не было 1 мин 17 с (с 14:05:00), причина: соединение оборвалось").Return(nil),
	)

	b.ReportTelegram(true, "")
	b.ReportTelegram(false, "error get updates: Post \"https://api.telegram.org/bot***/getUpdates\": http2: client connection lost")
	clock.advance(10 * time.Second)
	b.ReportTelegram(false, "context deadline exceeded")
	clock.advance(outageNoticeDelay)
	deps.timers.fire(0)
	clock.advance(37 * time.Second)
	b.ReportTelegram(true, "")
	b.ReportTelegram(true, "")
}

// TestReportTelegramShortOutage проверяет, что обрыв короче порога не порождает сообщений, а только считается.
func TestReportTelegramShortOutage(t *testing.T) {
	t.Parallel()

	clock := &movableClock{now: testNow}
	b, deps := newTestBot(t, withClock(clock.get))

	b.ReportTelegram(false, "unexpected EOF")
	clock.advance(5 * time.Second)
	b.ReportTelegram(true, "")

	deps.timers.fire(0)

	b.statsMu.Lock()
	defer b.statsMu.Unlock()

	if b.stats.telegramShort != 1 || len(b.stats.telegramOutages) != 0 {
		t.Fatalf("short = %d, long = %d, want 1 and 0", b.stats.telegramShort, len(b.stats.telegramOutages))
	}

	if got := deps.timers.duration(0); got != outageNoticeDelay {
		t.Fatalf("timer delay = %v, want %v", got, outageNoticeDelay)
	}
}

// TestReportMQTT проверяет сообщения о потере и восстановлении связи с брокером без повторов.
func TestReportMQTT(t *testing.T) {
	t.Parallel()

	clock := &movableClock{now: testNow}
	b, deps := newTestBot(t, withClock(clock.get))

	gomock.InOrder(
		deps.sender.EXPECT().Send(int64(100), "⚠️ Нет связи с MQTT-брокером контроллера: команды не выполняются").Return(nil),
		deps.sender.EXPECT().Send(int64(100), "✅ Связь с MQTT-брокером восстановлена. Не было 30 с (с 14:05:00)").Return(nil),
	)

	b.ReportMQTT(true)
	b.ReportMQTT(false)
	b.ReportMQTT(false)
	clock.advance(30 * time.Second)
	b.ReportMQTT(true)
	b.ReportMQTT(true)
}

// TestStatusAccess проверяет, что /status доступен только пользователям с системными уведомлениями.
func TestStatusAccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		chatID int64
		want   string
	}{
		{name: "without notifications", chatID: 200, want: "Неизвестная команда.\n\nДоступные команды:\n/gate — Открыть ворота"},
		{name: "with notifications", chatID: 100, want: "🤖 wb-telegram"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, deps := newTestBot(t)

			deps.sender.EXPECT().Stats().Return(telegram.Stats{Connected: true}).AnyTimes()
			deps.publisher.EXPECT().Connected().Return(true).AnyTimes()
			deps.sender.EXPECT().Send(tt.chatID, gomock.Any()).DoAndReturn(func(_ int64, text string) error {
				if !strings.HasPrefix(text, tt.want) {
					t.Errorf("reply = %q, want prefix %q", text, tt.want)
				}

				return nil
			})

			b.HandleMessage(context.Background(), message(1, tt.chatID, "/status"))
		})
	}
}

// TestStatusText проверяет сводку /status после команд, обрыва Telegram и переподключения MQTT.
func TestStatusText(t *testing.T) {
	t.Parallel()

	clock := &movableClock{now: testNow}
	b, deps := newTestBot(t, withClock(clock.get), WithVersion("0.1.16"))

	deps.sender.EXPECT().Send(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	deps.sender.EXPECT().Stats().Return(telegram.Stats{Sent: 42, Dropped: 1, Queued: 2, Connected: true})
	deps.publisher.EXPECT().Connected().Return(true).AnyTimes()
	deps.publisher.EXPECT().SetValue(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	clock.advance(2 * time.Hour)

	b.HandleMessage(context.Background(), staleMessage(clock.get()))
	b.HandleMessage(context.Background(), messageAt(5, 100, "/gate", clock.get()))
	deps.timers.fire(0)
	b.HandleMessage(context.Background(), messageAt(6, 100, "/boiler", clock.get()))
	clock.advance(400 * time.Millisecond)
	b.HandleSend([]byte(`{"text":"ok","reply_to":"6"}`))

	b.ReportTelegram(false, "net/http: TLS handshake timeout")
	clock.advance(77 * time.Second)
	deps.timers.fire(deps.timers.count() - 1)
	b.ReportTelegram(true, "")
	b.ReportTelegram(false, "unexpected EOF")
	clock.advance(3 * time.Second)
	b.ReportTelegram(true, "")
	b.ReportMQTT(false)
	clock.advance(time.Second)
	b.ReportMQTT(true)

	got := b.statusText()

	for _, want := range []string{
		"🤖 wb-telegram 0.1.16",
		"Работает 2 ч 1 мин (с 14:05)",
		"Telegram: ✅ связь есть",
		"  обрывов за сутки: 1, последний 16:05, длился 1 мин 17 с",
		"  Кратких обрывов с запуска: 1",
		"MQTT: ✅ подключено",
		"  переподключений: 1, последнее 16:06",
		"Сообщения: отправлено 42, не доставлено 1, в очереди 2",
		"Команды: вызовов 2, без ответа правил 1, устаревших 1",
		"  последняя: /boiler — Супруга, 16:05, ответ за 0,4 с",
		"Настройки: 2 пользователя, 2 команды; таймауты: подключение 30 с, опрос 30 с, ответ правила 10 с",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status has no line %q:\n%s", want, got)
		}
	}

	if strings.Contains(got, "mosquitto") || strings.Contains(got, "unix://") {
		t.Errorf("status must not show the broker address:\n%s", got)
	}
}

// messageAt — сообщение Telegram с заданным временем отправки.
func messageAt(updateID, chatID int64, text string, at time.Time) telegram.Message {
	return telegram.Message{UpdateID: updateID, ChatID: chatID, Text: text, Date: at}
}

// staleMessage — команда, отправленная давно, чем таймаут ответа.
func staleMessage(now time.Time) telegram.Message {
	return messageAt(4, 100, "/gate", now.Add(-time.Hour))
}

// TestFormatDuration проверяет человекочитаемые длительности.
func TestFormatDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   time.Duration
		want string
	}{
		{in: 0, want: "0 с"},
		{in: 59 * time.Second, want: "59 с"},
		{in: 77 * time.Second, want: "1 мин 17 с"},
		{in: 2*time.Hour + 5*time.Minute, want: "2 ч 5 мин"},
		{in: 51 * time.Hour, want: "2 д 3 ч"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()

			if got := formatDuration(tt.in); got != tt.want {
				t.Fatalf("formatDuration(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestPlural проверяет склонение существительных после числа.
func TestPlural(t *testing.T) {
	t.Parallel()

	tests := []struct {
		n    int
		want string
	}{
		{n: 1, want: "1 команда"},
		{n: 2, want: "2 команды"},
		{n: 5, want: "5 команд"},
		{n: 11, want: "11 команд"},
		{n: 21, want: "21 команда"},
		{n: 112, want: "112 команд"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()

			if got := plural(tt.n, "команда", "команды", "команд"); got != tt.want {
				t.Fatalf("plural(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

// TestDescribeReason проверяет перевод ошибок связи в понятные причины.
func TestDescribeReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want string
	}{
		{raw: "", want: "неизвестна"},
		{raw: "Post ...: net/http: TLS handshake timeout", want: "долгое TLS-рукопожатие"},
		{raw: "Post ...: context deadline exceeded (Client.Timeout exceeded while awaiting headers)", want: "нет ответа от Telegram"},
		{raw: "Post ...: http2: client connection lost", want: "соединение оборвалось"},
		{raw: "Post ...: unexpected EOF", want: "соединение оборвалось"},
		{raw: "dial tcp: lookup api.telegram.org: no such host", want: "не найден адрес api.telegram.org (DNS)"},
		{raw: "dial tcp 149.154.166.110:443: connect: connection refused", want: "нет маршрута до Telegram"},
		{raw: "telegram: something odd: strange thing", want: "strange thing"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()

			if got := describeReason(tt.raw); got != tt.want {
				t.Fatalf("describeReason(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
