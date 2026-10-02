package telegram

import (
	"context"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/retry"
)

// Option настраивает Client.
type Option func(*options)

// options — настройки Client.
type options struct {
	serverURL       string
	queueSize       int
	perChatInterval time.Duration
	globalInterval  time.Duration
	retryBase       time.Duration
	retryMax        time.Duration
	onFatal         func(error)
	onDelivery      func(error)
	sleep           func(ctx context.Context, d time.Duration) bool
}

// defaultOptions возвращает настройки по умолчанию (лимиты Telegram: 1 сообщение/с в чат, 30/с всего).
func defaultOptions() options {
	return options{
		serverURL:       "https://api.telegram.org",
		queueSize:       100,
		perChatInterval: time.Second,
		globalInterval:  34 * time.Millisecond,
		retryBase:       time.Second,
		retryMax:        60 * time.Second,
		onFatal:         func(error) {},
		onDelivery:      func(error) {},
		sleep:           retry.Sleep,
	}
}

// Validate проверяет настройки.
func (o options) Validate() error {
	switch {
	case o.serverURL == "":
		return &Error{kind: kindInvalidArgument, Op: "validate", Message: "server url is required"}
	case o.queueSize < 1:
		return &Error{kind: kindInvalidArgument, Op: "validate", Message: "queue size must be positive"}
	case o.perChatInterval < 0 || o.globalInterval < 0:
		return &Error{kind: kindInvalidArgument, Op: "validate", Message: "rate limits must not be negative"}
	case o.retryBase <= 0 || o.retryMax < o.retryBase:
		return &Error{kind: kindInvalidArgument, Op: "validate", Message: "bad retry settings"}
	case o.onFatal == nil || o.onDelivery == nil || o.sleep == nil:
		return &Error{kind: kindInvalidArgument, Op: "validate", Message: "callbacks must not be nil"}
	}

	return nil
}

// WithServerURL задаёт адрес Bot API (для тестов — httptest.Server).
func WithServerURL(url string) Option {
	return func(o *options) { o.serverURL = url }
}

// WithQueueSize задаёт ёмкость исходящей очереди.
func WithQueueSize(size int) Option {
	return func(o *options) { o.queueSize = size }
}

// WithRateLimits задаёт минимальные интервалы между сообщениями в один чат и между любыми сообщениями.
func WithRateLimits(perChat, global time.Duration) Option {
	return func(o *options) {
		o.perChatInterval = perChat
		o.globalInterval = global
	}
}

// WithRetry задаёт паузы повторов при временных ошибках: экспонента от base до maxDelay.
// Повторы не ограничены по числу — сообщение повторяется до успеха или остановки клиента.
func WithRetry(base, maxDelay time.Duration) Option {
	return func(o *options) {
		o.retryBase = base
		o.retryMax = maxDelay
	}
}

// WithOnFatal задаёт реакцию на неустранимую ошибку (отклонённый токен). Вызывается не более
// одного раза, на внутренней горутине клиента (polling, отправка, deleteWebhook): f должна быть
// потокобезопасной, не блокироваться и не вызывать Shutdown/Stop синхронно — иначе взаимоблокировка.
func WithOnFatal(f func(error)) Option {
	return func(o *options) { o.onFatal = f }
}

// WithOnDelivery задаёт реакцию на результат доставки: nil — успех, ошибка — после каждой неудачной
// попытки, ErrQueueFull — при выбросе сообщения из переполненной очереди. Вызывается и из горутины,
// вызвавшей Send, и из цикла отправки: f должна быть потокобезопасной, не блокироваться и не
// вызывать Shutdown/Stop синхронно.
func WithOnDelivery(f func(error)) Option {
	return func(o *options) { o.onDelivery = f }
}

// withSleep подменяет ожидание (для тестов).
func withSleep(f func(ctx context.Context, d time.Duration) bool) Option {
	return func(o *options) { o.sleep = f }
}
