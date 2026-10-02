package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// deliveryLog собирает результаты onDelivery.
type deliveryLog struct {
	mu      sync.Mutex
	results []error
}

// add запоминает результат доставки.
func (d *deliveryLog) add(err error) {
	d.mu.Lock()

	d.results = append(d.results, err)

	d.mu.Unlock()
}

// snapshot возвращает копию результатов.
func (d *deliveryLog) snapshot() []error {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]error(nil), d.results...)
}

// noSleep — подмена ожидания без реальной паузы.
func noSleep(ctx context.Context, _ time.Duration) bool {
	return ctx.Err() == nil
}

// TestSendDelivers проверяет доставку и отчёт об успехе.
func TestSendDelivers(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	log := &deliveryLog{}
	client := startClient(t, api, func(context.Context, Message) {}, WithOnDelivery(log.add))

	errSend := client.Send(100, "Ворота открываются")
	if errSend != nil {
		t.Fatalf("Send: %v", errSend)
	}

	waitFor(t, "message sent", func() bool { return len(api.sentMessages()) == 1 })

	sent := api.sentMessages()[0]
	if sent.ChatID != "100" || sent.Text != "Ворота открываются" {
		t.Fatalf("sent = %+v", sent)
	}

	waitFor(t, "delivery reported", func() bool { return len(log.snapshot()) == 1 })

	if errDelivered := log.snapshot()[0]; errDelivered != nil {
		t.Fatalf("delivery error = %v", errDelivered)
	}
}

// TestSendTruncatesLongText проверяет, что длинный текст уходит обрезанным.
func TestSendTruncatesLongText(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	client := startClient(t, api, func(context.Context, Message) {})

	_ = client.Send(100, strings.Repeat("я", MaxMessageRunes+1))

	waitFor(t, "message sent", func() bool { return len(api.sentMessages()) == 1 })

	if text := api.sentMessages()[0].Text; text != truncate(strings.Repeat("я", MaxMessageRunes+1)) {
		t.Fatalf("sent text is not truncated: %d bytes", len(text))
	}
}

// TestSendRetries проверяет повторы того же сообщения при 429 и 5xx, отказ на 403 и
// отчёт onDelivery после каждой попытки; следующее сообщение после этого доставляется.
func TestSendRetries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		statuses    []string
		wantSent    int
		wantResults []error
	}{
		{name: "rate limited then ok", statuses: []string{"429", "ok"}, wantSent: 1, wantResults: []error{ErrRateLimited, nil}},
		{name: "server error then ok", statuses: []string{"500", "500", "ok"}, wantSent: 1, wantResults: []error{ErrTransient, ErrTransient, nil}},
		{
			name:        "long outage then ok",
			statuses:    []string{"500", "500", "500", "500", "500", "500", "ok"},
			wantSent:    1,
			wantResults: []error{ErrTransient, ErrTransient, ErrTransient, ErrTransient, ErrTransient, ErrTransient, nil},
		},
		{name: "blocked by user", statuses: []string{"403"}, wantSent: 0, wantResults: []error{ErrRejected}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			api := newFakeAPI(t)
			api.queueSendStatus(tt.statuses...)

			log := &deliveryLog{}
			client := startClient(t, api, func(context.Context, Message) {}, WithOnDelivery(log.add), withSleep(noSleep))

			_ = client.Send(100, "x")

			waitFor(t, "delivery reported", func() bool { return len(log.snapshot()) == len(tt.wantResults) })

			if got := len(api.sentMessages()); got != tt.wantSent {
				t.Fatalf("sent = %d, want %d", got, tt.wantSent)
			}

			for i, errDelivered := range log.snapshot() {
				want := tt.wantResults[i]
				if (want == nil) != (errDelivered == nil) || (want != nil && !errors.Is(errDelivered, want)) {
					t.Fatalf("delivery %d = %v, want %v", i, errDelivered, want)
				}
			}

			_ = client.Send(100, "next")

			waitFor(t, "next message sent", func() bool { return len(api.sentMessages()) == tt.wantSent+1 })
		})
	}
}

// TestSendOutageStopsOnShutdown проверяет, что при длительном сбое сообщение повторяется,
// пока клиент не остановлен, а Shutdown не зависает.
func TestSendOutageStopsOnShutdown(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)

	statuses := make([]string, 100000)
	for i := range statuses {
		statuses[i] = "500"
	}

	api.queueSendStatus(statuses...)

	log := &deliveryLog{}
	client := startClient(t, api, func(context.Context, Message) {}, WithOnDelivery(log.add))

	_ = client.Send(100, "x")

	waitFor(t, "several attempts", func() bool { return len(log.snapshot()) >= 5 })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	_ = client.Shutdown(ctx)

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %v", elapsed)
	}

	if got := len(api.sentMessages()); got != 0 {
		t.Fatalf("sent = %d, want 0", got)
	}

	for i, errDelivered := range log.snapshot() {
		if !errors.Is(errDelivered, ErrTransient) {
			t.Fatalf("delivery %d = %v, want ErrTransient", i, errDelivered)
		}
	}
}

// TestShutdownDeadlineDropsQueue проверяет, что истёкшая досылка очереди при остановке — не ошибка:
// Shutdown возвращает nil, а недоставленные сообщения (повторяемое и ждущие в очереди) подсчитаны.
func TestShutdownDeadlineDropsQueue(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)

	statuses := make([]string, 100000)
	for i := range statuses {
		statuses[i] = "500"
	}

	api.queueSendStatus(statuses...)

	log := &deliveryLog{}
	client := startClient(t, api, func(context.Context, Message) {}, WithOnDelivery(log.add))

	for _, text := range []string{"a", "b", "c"} {
		errSend := client.Send(100, text)
		if errSend != nil {
			t.Fatalf("Send: %v", errSend)
		}
	}

	waitFor(t, "first attempts", func() bool { return len(log.snapshot()) >= 2 })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	errShutdown := client.Shutdown(ctx)
	if errShutdown != nil {
		t.Fatalf("Shutdown = %v, want nil", errShutdown)
	}

	if got := client.undelivered(); got != 3 {
		t.Fatalf("undelivered = %d, want 3", got)
	}
}

// TestSendUnauthorizedIsFatal проверяет onFatal при 401 на отправке.
func TestSendUnauthorizedIsFatal(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	api.queueSendStatus("401")

	fatal := make(chan error, 4)
	client := startClient(t, api, func(context.Context, Message) {}, WithOnFatal(func(err error) {
		select {
		case fatal <- err:
		default:
		}
	}))

	_ = client.Send(100, "x")

	select {
	case err := <-fatal:
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("fatal = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onFatal was not called")
	}
}

// TestSendPerChatRateLimit проверяет интервал между сообщениями в один чат.
func TestSendPerChatRateLimit(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	client := startClient(t, api, func(context.Context, Message) {}, WithRateLimits(200*time.Millisecond, time.Millisecond))

	_ = client.Send(100, "1")
	_ = client.Send(100, "2")

	waitFor(t, "two messages", func() bool { return len(api.sentMessages()) == 2 })

	sent := api.sentMessages()
	if gap := sent[1].At.Sub(sent[0].At); gap < 180*time.Millisecond {
		t.Fatalf("gap between messages to one chat = %v, want >= 200ms", gap)
	}
}

// TestSendQueueFullAndClosed проверяет ErrQueueFull, отчёт onDelivery о выброшенном и ErrClosed.
func TestSendQueueFullAndClosed(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	log := &deliveryLog{}
	client := startClient(t, api, func(context.Context, Message) {},
		WithQueueSize(1), WithRateLimits(time.Hour, time.Millisecond), WithOnDelivery(log.add))

	_ = client.Send(100, "first")

	waitFor(t, "first sent", func() bool { return len(api.sentMessages()) == 1 })

	var sawFull bool

	for _, text := range []string{"second", "third", "fourth"} {
		if errors.Is(client.Send(100, text), ErrQueueFull) {
			sawFull = true
		}
	}

	if !sawFull {
		t.Fatal("Send never returned ErrQueueFull")
	}

	var reportedFull bool

	for _, errDelivered := range log.snapshot() {
		if errors.Is(errDelivered, ErrQueueFull) {
			reportedFull = true
		}
	}

	if !reportedFull {
		t.Fatal("onDelivery was not told about ErrQueueFull")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_ = client.Shutdown(ctx)

	errClosed := client.Send(100, "after stop")
	if !errors.Is(errClosed, ErrClosed) {
		t.Fatalf("Send after Shutdown = %v, want ErrClosed", errClosed)
	}
}

// TestSendAfterShutdownWithoutRun проверяет ErrClosed, если клиент остановлен, не будучи запущенным.
func TestSendAfterShutdownWithoutRun(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)

	client, errNew := New(testToken, func(context.Context, Message) {}, WithServerURL(api.server.URL))
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	_ = client.Stop()

	errClosed := client.Send(100, "x")
	if !errors.Is(errClosed, ErrClosed) {
		t.Fatalf("Send after Stop = %v, want ErrClosed", errClosed)
	}
}

// TestRetryOptionValidation проверяет проверку настроек повторов.
func TestRetryOptionValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		base     time.Duration
		maxDelay time.Duration
		wantErr  bool
	}{
		{name: "valid", base: time.Second, maxDelay: time.Minute},
		{name: "zero base", base: 0, maxDelay: time.Minute, wantErr: true},
		{name: "max below base", base: time.Minute, maxDelay: time.Second, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, errNew := New(testToken, func(context.Context, Message) {}, WithRetry(tt.base, tt.maxDelay))
			if (errNew != nil) != tt.wantErr {
				t.Fatalf("New error = %v, wantErr %v", errNew, tt.wantErr)
			}

			if tt.wantErr && !errors.Is(errNew, ErrInvalidArgument) {
				t.Fatalf("New error = %v, want ErrInvalidArgument", errNew)
			}
		})
	}
}

// fatalCounter считает вызовы onFatal.
type fatalCounter struct {
	calls atomic.Int32
}

// onFatal увеличивает счётчик.
func (f *fatalCounter) onFatal(error) {
	f.calls.Add(1)
}

// queueLen возвращает число сообщений, ожидающих в очереди.
func queueLen(c *Client) int {
	c.queue.mu.Lock()
	defer c.queue.mu.Unlock()

	return len(c.queue.items)
}

// TestSendUnauthorizedFatalOnce проверяет: при 401 onFatal вызывается один раз, остальные
// сообщения выбрасываются без отправки и без повторных onFatal/onDelivery.
func TestSendUnauthorizedFatalOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(api *fakeAPI)
	}{
		{name: "send 401", setup: func(api *fakeAPI) { api.queueSendStatus("401", "401", "401") }},
		{name: "poll and send 401", setup: func(api *fakeAPI) { api.setUnauthorized() }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			api := newFakeAPI(t)
			tt.setup(api)

			counter := &fatalCounter{}
			log := &deliveryLog{}
			client := startClient(t, api, func(context.Context, Message) {},
				WithOnFatal(counter.onFatal), WithOnDelivery(log.add), withSleep(noSleep))

			for _, text := range []string{"1", "2", "3"} {
				_ = client.Send(100, text)
			}

			waitFor(t, "queue drained", func() bool { return queueLen(client) == 0 && len(log.snapshot()) >= 1 })

			time.Sleep(200 * time.Millisecond)

			if got := counter.calls.Load(); got != 1 {
				t.Fatalf("onFatal calls = %d, want 1", got)
			}

			results := log.snapshot()
			if len(results) != 1 || !errors.Is(results[0], ErrUnauthorized) {
				t.Fatalf("deliveries = %v, want one ErrUnauthorized", results)
			}

			if got := len(api.sentMessages()); got != 0 {
				t.Fatalf("sent = %d, want 0", got)
			}
		})
	}
}
