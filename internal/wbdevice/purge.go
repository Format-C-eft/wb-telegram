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

// purgeClientID возвращает уникальный client id уборки: брокер отключает прежнего клиента
// с тем же id, поэтому одновременные уборки (или уборка и запущенный экземпляр) не должны его делить.
// Id не длиннее 23 символов — столько MQTT 3.1.1 гарантирует любому брокеру.
func purgeClientID() string {
	return "tgpurge-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(purgeSeq.Add(1), 10)
}

// Purge удаляет устройство из брокера целиком: публикует пустые retained-сообщения во все его топики,
// найденные за окно window.
func Purge(broker, deviceID string, window time.Duration) error {
	if !identifierPattern.MatchString(deviceID) {
		return &Error{kind: kindInvalidArgument, Op: "purge", Message: "bad device id " + deviceID}
	}

	clientOptions := paho.NewClientOptions().
		AddBroker(broker).
		SetClientID(purgeClientID()).
		SetCleanSession(true).
		SetConnectTimeout(purgeTimeout)

	client := paho.NewClient(clientOptions)

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

// removeStale удаляет из брокера контролы устройства, которых нет в текущем наборе.
// Уборка не обязательна для работы устройства, поэтому ошибки только пишутся в журнал.
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

// collectRetained подписывается на filter и собирает топики с непустыми retained-сообщениями за окно window.
// Если stop закрывается раньше конца окна, сбор прерывается и возвращается пустой список.
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

	if !token.WaitTimeout(purgeTimeout) {
		return nil, &Error{kind: kindTimeout, Op: "subscribe", Topic: filter, Message: "no answer from broker"}
	}

	if token.Error() != nil {
		return nil, &Error{kind: kindMQTT, Op: "subscribe", Topic: filter, Message: token.Error().Error()}
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
