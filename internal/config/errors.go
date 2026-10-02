// Package config читает, проверяет и готовит для UI конфигурацию wb-telegram.
package config

// errorKind классифицирует ошибки пакета config.
type errorKind int

const (
	kindUnknown errorKind = iota
	kindInvalid
	kindNoToken
	kindIO
)

// Error — ошибка пакета config.
type Error struct {
	kind    errorKind
	Op      string
	Path    string
	Message string
}

// Error возвращает текст ошибки.
func (e *Error) Error() string {
	text := "config"

	if e.Op != "" {
		text += ": " + e.Op
	}

	if e.Path != "" {
		text += " " + e.Path
	}

	return text + ": " + e.Message
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
	// ErrInvalid сообщает о некорректном конфиге или вводе из формы.
	ErrInvalid = &Error{kind: kindInvalid, Message: "invalid config"}
	// ErrNoToken сообщает, что токен не задан.
	ErrNoToken = &Error{kind: kindNoToken, Message: "token is not set"}
	// ErrIO сообщает об ошибке чтения или записи файла.
	ErrIO = &Error{kind: kindIO, Message: "i/o error"}
)
