package app

import (
	"context"
	"os/signal"
	"syscall"
	"time"
)

// Option настраивает App.
type Option func(*options)

// options — настройки App.
type options struct {
	shutdownTimeout time.Duration
	signalContext   func() (context.Context, context.CancelFunc)
}

// defaultOptions возвращает настройки по умолчанию.
func defaultOptions() options {
	return options{
		shutdownTimeout: 5 * time.Second,
		signalContext: func() (context.Context, context.CancelFunc) {
			return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		},
	}
}

// Validate проверяет настройки.
func (o options) Validate() error {
	if o.shutdownTimeout <= 0 {
		return &Error{kind: kindInvalidOption, Op: "validate", Message: "shutdown timeout must be positive"}
	}

	if o.signalContext == nil {
		return &Error{kind: kindInvalidOption, Op: "validate", Message: "signal context is required"}
	}

	return nil
}

// WithShutdownTimeout задаёт общий таймаут остановки компонентов.
func WithShutdownTimeout(d time.Duration) Option {
	return func(o *options) {
		o.shutdownTimeout = d
	}
}

// withSignalContext подменяет источник сигналов остановки (для тестов).
func withSignalContext(f func() (context.Context, context.CancelFunc)) Option {
	return func(o *options) {
		o.signalContext = f
	}
}
