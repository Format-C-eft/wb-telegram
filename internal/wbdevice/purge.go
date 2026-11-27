package wbdevice

import (
	"log/slog"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// purgeTimeout — таймаут операций MQTT при уборке.
const purgeTimeout = 5 * time.Second

// purgeSeq делает client id уборок уникальными в пределах процесса.
var purgeSeq atomic.Int64

// purgeClientID возвращает уникальный идентификатор MQTT-клиента уборки.
func purgeClientID() string {
	return "tgpurge-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(purgeSeq.Add(1), 10)
}

// Purge удаляет retained-топики устройства, найденные за заданное окно.
func Purge(broker, deviceID string, window time.Duration) error {
	return purge(broker, deviceID, window, paho.NewClient)
}

// purge удаляет устройство через заданную фабрику MQTT-клиента.
func purge(broker, deviceID string, window time.Duration, factory func(*paho.ClientOptions) paho.Client) error {
	if !identifierPattern.MatchString(deviceID) {
		return &Error{kind: kindInvalidArgument, Op: "purge", Message: "bad device id " + deviceID}
	}

	clientOptions := paho.NewClientOptions().
		AddBroker(broker).
		SetClientID(purgeClientID()).
		SetCleanSession(true).
		SetConnectTimeout(purgeTimeout)

	client := factory(clientOptions)

	token := client.Connect()
	if !token.WaitTimeout(purgeTimeout) {
		return &Error{kind: kindTimeout, Op: "purge connect", Message: broker}
	}

	if token.Error() != nil {
		return &Error{kind: kindMQTT, Op: "purge connect", Message: token.Error().Error()}
	}

	defer client.Disconnect(250)

	topics, errCollect := collectRetained(client, devicesPrefix+deviceID+"/#", window, nil)
	if errCollect != nil {
		return errCollect
	}

	return clearTopics(client, topics)
}

// removeStale удаляет retained-топики контролов, отсутствующих в текущем наборе.
func (d *Device) removeStale() {
	topics, errCollect := collectRetained(d.client, devicesPrefix+d.id+"/controls/#", d.options.staleWindow, d.stop)
	if errCollect != nil {
		slog.Warn("MQTT: не удалось собрать устаревшие контролы", "err", errCollect)

		return
	}

	var stale []string

	for _, topic := range topics {
		controlID, ok := controlIDFromTopic(d.id, topic)
		if !ok {
			continue
		}

		if _, known := d.controls[controlID]; !known {
			stale = append(stale, topic)
		}
	}

	errClear := clearTopics(d.client, stale)
	if errClear != nil {
		slog.Warn("MQTT: не удалось удалить устаревшие контролы", "err", errClear)

		return
	}

	if len(stale) > 0 {
		slog.Info("MQTT: удалены контролы удалённых команд", "topics", len(stale))
	}
}

// collectRetained собирает непустые retained-топики указанного фильтра.
func collectRetained(client paho.Client, filter string, window time.Duration, stop <-chan struct{}) ([]string, error) {
	var (
		mu     sync.Mutex
		topics = map[string]bool{}
	)

	token := client.Subscribe(filter, 1, func(_ paho.Client, message paho.Message) {
		if !message.Retained() || len(message.Payload()) == 0 {
			return
		}

		mu.Lock()

		topics[message.Topic()] = true

		mu.Unlock()
	})

	errSubscribe := waitSubscribed(token, filter, purgeTimeout)
	if errSubscribe != nil {
		return nil, errSubscribe
	}

	stopped := false

	select {
	case <-stop:
		stopped = true
	case <-time.After(window):
	}

	unsubscribe := client.Unsubscribe(filter)
	unsubscribe.WaitTimeout(purgeTimeout)

	if stopped {
		return nil, nil
	}

	mu.Lock()
	defer mu.Unlock()

	out := make([]string, 0, len(topics))

	for topic := range topics {
		out = append(out, topic)
	}

	slices.Sort(out)

	return out, nil
}

// clearTopics удаляет retained-сообщения: публикует в каждый топик пустой retained payload.
func clearTopics(client paho.Client, topics []string) error {
	for _, topic := range topics {
		token := client.Publish(topic, 1, true, "")
		if !token.WaitTimeout(purgeTimeout) {
			return &Error{kind: kindTimeout, Op: "clear", Topic: topic, Message: "no answer from broker"}
		}

		if token.Error() != nil {
			return &Error{kind: kindMQTT, Op: "clear", Topic: topic, Message: token.Error().Error()}
		}
	}

	return nil
}
