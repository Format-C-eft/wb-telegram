package wb_history

import (
	"net/url"
	"slices"
)

// Option настраивает клиент истории.
type Option func(*options)
type options struct{ broker string }

// Validate проверяет адрес брокера.
func (o options) Validate() error {
	parsed, errParse := url.Parse(o.broker)
	if errParse != nil {
		return ErrInvalidArgument
	}

	if !slices.Contains([]string{"tcp", "ssl", "tls", "ws", "wss", "unix"}, parsed.Scheme) || (parsed.Host == "" && parsed.Path == "") {
		return ErrInvalidArgument
	}

	return nil
}

// WithBroker задаёт адрес MQTT-брокера.
func WithBroker(broker string) Option { return func(o *options) { o.broker = broker } }
