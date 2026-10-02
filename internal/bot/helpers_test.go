package bot

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/bot/mock"
	"github.com/Format-C-eft/wb-telegram/internal/config"
)

// testNow — «текущее» время тестов.
var testNow = time.Date(2026, 10, 1, 14, 5, 0, 0, time.UTC)

// testConfig возвращает конфиг: Супруга (100, уведомления), Ребёнок (200); /gate — всем, /boiler — Супруге.
func testConfig() *config.Config {
	return &config.Config{
		Enabled:       true,
		ReplyTimeoutS: 10,
		Users: []config.User{
			{Name: "Супруга", ChatID: 100, Notifications: true},
			{Name: "Ребёнок", ChatID: 200},
		},
		Commands: []config.Command{
			{Name: "gate", Description: "Открыть ворота", Users: []string{}},
			{Name: "boiler", Description: "Состояние котла", Users: []string{"Супруга"}},
		},
	}
}

// fakeTimers — управляемые вручную таймеры вместо time.AfterFunc.
type fakeTimers struct {
	mu        sync.Mutex
	fns       []func()
	durations []time.Duration
	stopped   []bool
}

// afterFunc запоминает функцию и задержку; срабатывает только через fire.
func (f *fakeTimers) afterFunc(d time.Duration, fn func()) func() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	index := len(f.fns)
	f.fns = append(f.fns, fn)
	f.durations = append(f.durations, d)
	f.stopped = append(f.stopped, false)

	return func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()

		wasActive := !f.stopped[index]
		f.stopped[index] = true

		return wasActive
	}
}

// fire запускает таймер index, если он не остановлен.
func (f *fakeTimers) fire(index int) {
	f.mu.Lock()

	fn := f.fns[index]
	active := !f.stopped[index]
	f.stopped[index] = true

	f.mu.Unlock()

	if active {
		fn()
	}
}

// count возвращает число созданных таймеров.
func (f *fakeTimers) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.fns)
}

// duration возвращает задержку таймера index.
func (f *fakeTimers) duration(index int) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.durations[index]
}

// testDeps — моки и таймеры одного теста.
type testDeps struct {
	publisher *mock.MockPublisher
	sender    *mock.MockSender
	timers    *fakeTimers
}

// newTestBot создаёт Bot с моками, фиксированным временем, ручными таймерами и мгновенным sleep;
// opts применяются последними и переопределяют эти настройки.
func newTestBot(t *testing.T, opts ...Option) (*Bot, testDeps) {
	t.Helper()

	ctrl := gomock.NewController(t)
	deps := testDeps{
		publisher: mock.NewMockPublisher(ctrl),
		sender:    mock.NewMockSender(ctrl),
		timers:    &fakeTimers{},
	}

	base := []Option{
		withClock(func() time.Time { return testNow }),
		withAfterFunc(deps.timers.afterFunc),
		withSleep(func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }),
		withLocation(time.UTC),
	}

	b, errNew := New(testConfig(), deps.publisher, deps.sender, append(base, opts...)...)
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	return b, deps
}
