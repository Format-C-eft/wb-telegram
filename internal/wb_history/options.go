package wb_history

import (
	"slices"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// Option настраивает клиент истории.
type Option func(*options)
type options struct{ broker string }

// Validate проверяет адрес брокера.
func (o options) Validate() error {
	if o.broker == "" {
		return ErrInvalidArgument
	}

	servers := paho.NewClientOptions().AddBroker(o.broker).Servers
	if len(servers) != 1 {
		return ErrInvalidArgument
	}

	server := servers[0]
	if !slices.Contains([]string{"tcp", "mqtt", "ssl", "tls", "mqtts", "mqtt+ssl", "tcps", "ws", "wss", "unix"}, server.Scheme) || (server.Host == "" && server.Path == "") {
		return ErrInvalidArgument
	}

	return nil
}

// WithBroker задаёт адрес MQTT-брокера.
func WithBroker(broker string) Option { return func(o *options) { o.broker = broker } }
