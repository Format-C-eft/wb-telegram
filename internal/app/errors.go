// Package app управляет жизненным циклом компонентов сервиса.
package app

// errorKind классифицирует ошибки пакета app.
type errorKind int

const (
	kindUnknown errorKind = iota
	kindNilComponent
	kindAlreadyRunning
	kindInvalidOption
)

// Error — ошибка пакета app.
type Error struct {
	kind    errorKind
	Op      string
	Message string
}

// Error возвращает текст ошибки.
func (e *Error) Error() string {
	if e.Op == "" {
		return "app: " + e.Message
	}

	return "app: " + e.Op + ": " + e.Message
}

// Is сравнивает ошибки по виду, чтобы работал errors.Is с сентинелами.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}

	return e.kind == t.kind
}

var (
	// ErrNilComponent сообщает о регистрации nil-компонента.
	ErrNilComponent = &Error{kind: kindNilComponent, Message: "nil component"}
	// ErrAlreadyRunning сообщает о повторном или конкурентном запуске App.
	ErrAlreadyRunning = &Error{kind: kindAlreadyRunning, Message: "already running"}
	// ErrInvalidOption сообщает о недопустимой опции App.
	ErrInvalidOption = &Error{kind: kindInvalidOption, Message: "invalid option"}
)
