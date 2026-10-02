// Package telegram — клиент Telegram Bot API для wb-telegram поверх go-telegram/bot.
package telegram

import (
	"errors"
	"time"

	"github.com/go-telegram/bot"
)

// errorKind классифицирует ошибки пакета telegram.
type errorKind int

const (
	kindUnknown errorKind = iota
	kindUnauthorized
	kindRateLimited
	kindRejected
	kindTransient
	kindQueueFull
	kindClosed
	kindInvalidArgument
)

// Error — ошибка пакета telegram; текст никогда не содержит токен.
type Error struct {
	kind       errorKind
	Op         string
	Message    string
	RetryAfter time.Duration
}

// Error возвращает текст ошибки.
func (e *Error) Error() string {
	if e.Op == "" {
		return "telegram: " + e.Message
	}

	return "telegram: " + e.Op + ": " + e.Message
}

// Is сравнивает ошибки по виду.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}

	return e.kind == t.kind
}

var (
	// ErrUnauthorized сообщает, что Telegram отклонил токен.
	ErrUnauthorized = &Error{kind: kindUnauthorized, Message: "unauthorized"}
	// ErrRateLimited сообщает об ответе 429; RetryAfter — сколько ждать.
	ErrRateLimited = &Error{kind: kindRateLimited, Message: "too many requests"}
	// ErrRejected сообщает об окончательном отказе (400/403/404), повтор бессмыслен.
	ErrRejected = &Error{kind: kindRejected, Message: "rejected"}
	// ErrTransient сообщает о временной ошибке (сеть, 5xx), можно повторить.
	ErrTransient = &Error{kind: kindTransient, Message: "temporary failure"}
	// ErrQueueFull сообщает, что очередь переполнена и самое старое сообщение выброшено.
	ErrQueueFull = &Error{kind: kindQueueFull, Message: "queue is full, oldest message dropped"}
	// ErrClosed сообщает, что клиент остановлен.
	ErrClosed = &Error{kind: kindClosed, Message: "client is closed"}
	// ErrInvalidArgument сообщает о некорректном аргументе или опции.
	ErrInvalidArgument = &Error{kind: kindInvalidArgument, Message: "invalid argument"}
)

// translate переводит ошибку go-telegram/bot в *Error пакета с замаскированным текстом.
func translate(op string, err error, mask func(string) string) *Error {
	message := mask(err.Error())

	var tooMany *bot.TooManyRequestsError

	switch {
	case errors.As(err, &tooMany):
		return &Error{kind: kindRateLimited, Op: op, Message: message, RetryAfter: time.Duration(tooMany.RetryAfter) * time.Second}
	case errors.Is(err, bot.ErrorUnauthorized):
		return &Error{kind: kindUnauthorized, Op: op, Message: message}
	case errors.Is(err, bot.ErrorForbidden), errors.Is(err, bot.ErrorBadRequest), errors.Is(err, bot.ErrorNotFound):
		return &Error{kind: kindRejected, Op: op, Message: message}
	default:
		return &Error{kind: kindTransient, Op: op, Message: message}
	}
}
