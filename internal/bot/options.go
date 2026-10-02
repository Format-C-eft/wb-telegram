package bot

import (
	"context"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/retry"
)

// Option настраивает Bot.
type Option func(*options)

// options — настройки Bot.
type options struct {
	now        func() time.Time
	afterFunc  func(d time.Duration, f func()) func() bool
	sleep      func(ctx context.Context, d time.Duration) bool
	location   *time.Location
	pendingTTL time.Duration
	retryBase  time.Duration
	retryMax   time.Duration
}

// defaultOptions возвращает настройки по умолчанию.
func defaultOptions() options {
	return options{
		now: time.Now,
		afterFunc: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
		sleep:      retry.Sleep,
		location:   time.Local,
		pendingTTL: time.Hour,
		retryBase:  time.Second,
		retryMax:   60 * time.Second,
	}
}

// Validate проверяет настройки.
func (o options) Validate() error {
	if o.now == nil || o.afterFunc == nil || o.sleep == nil || o.location == nil {
		return &Error{kind: kindInvalidArgument, Op: "validate", Message: "clock, timers, sleep and location are required"}
	}

	if o.pendingTTL <= 0 || o.retryBase <= 0 || o.retryMax < o.retryBase {
		return &Error{kind: kindInvalidArgument, Op: "validate", Message: "bad durations"}
	}

	return nil
}

// withClock подменяет источник текущего времени (для тестов).
func withClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// withAfterFunc подменяет таймеры (для тестов).
func withAfterFunc(f func(d time.Duration, fn func()) func() bool) Option {
	return func(o *options) { o.afterFunc = f }
}

// withSleep подменяет ожидание (для тестов).
func withSleep(f func(ctx context.Context, d time.Duration) bool) Option {
	return func(o *options) { o.sleep = f }
}

// withLocation задаёт часовой пояс для времени в сообщениях (для тестов).
func withLocation(location *time.Location) Option {
	return func(o *options) { o.location = location }
}
