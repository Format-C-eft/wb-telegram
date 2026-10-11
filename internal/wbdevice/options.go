package wbdevice

import (
	"time"
)

// Option настраивает Device.
type Option func(*options)

// options — настройки Device.
type options struct {
	broker      string
	clientID    string
	driver      string
	title       map[string]string
	controls    []Control
	onWrite     WriteHandler
	onConnected func(ok bool)
	timeout     time.Duration
	staleWindow time.Duration
}

// defaultOptions возвращает настройки по умолчанию.
func defaultOptions() options {
	return options{
		broker:      DefaultBroker(MosquittoSocket),
		clientID:    "wb-telegram",
		driver:      "wb-telegram",
		title:       map[string]string{"en": "Telegram Bot", "ru": "Telegram-бот"},
		timeout:     10 * time.Second,
		staleWindow: time.Second,
		onConnected: func(bool) {},
	}
}

// Validate проверяет настройки.
func (o options) Validate() error {
	switch {
	case o.broker == "":
		return &Error{kind: kindInvalidArgument, Op: "validate", Message: "broker is required"}
	case o.clientID == "":
		return &Error{kind: kindInvalidArgument, Op: "validate", Message: "client id is required"}
	case o.driver == "":
		return &Error{kind: kindInvalidArgument, Op: "validate", Message: "driver is required"}
	case o.onConnected == nil:
		return &Error{kind: kindInvalidArgument, Op: "validate", Message: "connection handler must not be nil"}
	case o.timeout <= 0 || o.staleWindow <= 0:
		return &Error{kind: kindInvalidArgument, Op: "validate", Message: "timeouts must be positive"}
	}

	seen := map[string]bool{}

	for _, control := range o.controls {
		if !identifierPattern.MatchString(control.ID) {
			return &Error{kind: kindInvalidArgument, Op: "validate", Message: "bad control id " + control.ID}
		}

		if seen[control.ID] {
			return &Error{kind: kindInvalidArgument, Op: "validate", Message: "duplicate control " + control.ID}
		}

		seen[control.ID] = true
	}

	return nil
}

// WithOnConnection задаёт реакцию на связь с брокером: ok=true, когда устройство опубликовано
// и готово (в том числе после переподключения), ok=false при потере связи. Вызывается на
// горутинах paho: обработчик должен быть потокобезопасным и не блокироваться.
func WithOnConnection(f func(ok bool)) Option {
	return func(o *options) { o.onConnected = f }
}

// WithBroker задаёт адрес брокера (tcp://host:port или unix:///path).
func WithBroker(broker string) Option {
	return func(o *options) { o.broker = broker }
}

// WithClientID задаёт MQTT client id.
func WithClientID(clientID string) Option {
	return func(o *options) { o.clientID = clientID }
}

// WithDriver задаёт имя драйвера в meta устройства.
func WithDriver(driver string) Option {
	return func(o *options) { o.driver = driver }
}

// WithTitle задаёт заголовок устройства на английском и русском.
func WithTitle(en, ru string) Option {
	return func(o *options) { o.title = map[string]string{"en": en, "ru": ru} }
}

// WithControls задаёт контролы устройства (порядок важен для UI).
func WithControls(controls ...Control) Option {
	return func(o *options) { o.controls = append(o.controls, controls...) }
}

// WithWriteHandler задаёт обработчик записей в /on.
func WithWriteHandler(handler WriteHandler) Option {
	return func(o *options) { o.onWrite = handler }
}

// WithTimeout задаёт таймаут операций MQTT.
func WithTimeout(timeout time.Duration) Option {
	return func(o *options) { o.timeout = timeout }
}

// WithStaleWindow задаёт окно сбора retained-сообщений при уборке устаревших контролов.
func WithStaleWindow(window time.Duration) Option {
	return func(o *options) { o.staleWindow = window }
}
