// Package bot — ядро wb-telegram: доступ, команды, маршрутизация ответов правил.
package bot

// errorKind классифицирует ошибки пакета bot.
type errorKind int

const (
	kindUnknown errorKind = iota
	kindInvalidArgument
	kindBadPayload
	kindInvalidState
)

// Error — ошибка пакета bot.
type Error struct {
	kind    errorKind
	Op      string
	Message string
}

// Error возвращает текст ошибки.
func (e *Error) Error() string {
	if e.Op == "" {
		return "bot: " + e.Message
	}

	return "bot: " + e.Op + ": " + e.Message
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
	// ErrInvalidArgument сообщает о некорректном аргументе конструктора или опции.
	ErrInvalidArgument = &Error{kind: kindInvalidArgument, Message: "invalid argument"}
	// ErrBadPayload сообщает о некорректном сообщении от правил.
	ErrBadPayload = &Error{kind: kindBadPayload, Message: "bad payload"}
	// ErrInvalidState сообщает о вызове в неподходящем состоянии (повторный Run, Run после Stop).
	ErrInvalidState = &Error{kind: kindInvalidState, Message: "invalid state"}
)
