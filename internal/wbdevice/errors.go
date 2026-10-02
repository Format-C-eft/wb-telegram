// Package wbdevice публикует устройство в MQTT по Wiren Board MQTT Conventions.
package wbdevice

// errorKind классифицирует ошибки пакета wbdevice.
type errorKind int

const (
	kindUnknown errorKind = iota
	kindNotConnected
	kindUnknownControl
	kindInvalidArgument
	kindTimeout
	kindMQTT
)

// Error — ошибка пакета wbdevice.
type Error struct {
	kind    errorKind
	Op      string
	Topic   string
	Message string
}

// Error возвращает текст ошибки.
func (e *Error) Error() string {
	text := "wbdevice"

	if e.Op != "" {
		text += ": " + e.Op
	}

	if e.Topic != "" {
		text += " " + e.Topic
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
	// ErrNotConnected сообщает, что соединения с брокером нет.
	ErrNotConnected = &Error{kind: kindNotConnected, Message: "not connected"}
	// ErrUnknownControl сообщает об обращении к незарегистрированному контролу.
	ErrUnknownControl = &Error{kind: kindUnknownControl, Message: "unknown control"}
	// ErrInvalidArgument сообщает о некорректном идентификаторе или опции.
	ErrInvalidArgument = &Error{kind: kindInvalidArgument, Message: "invalid argument"}
	// ErrTimeout сообщает, что брокер не ответил вовремя.
	ErrTimeout = &Error{kind: kindTimeout, Message: "timeout"}
	// ErrMQTT сообщает об ошибке операции MQTT.
	ErrMQTT = &Error{kind: kindMQTT, Message: "mqtt error"}
)
