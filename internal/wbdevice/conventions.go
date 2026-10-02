package wbdevice

import (
	"os"
	"regexp"
	"strings"
)

const (
	// MosquittoSocket — unix-сокет брокера на контроллере Wiren Board.
	MosquittoSocket = "/var/run/mosquitto/mosquitto.sock"
	// ControlTypeText — тип контрола «текст».
	ControlTypeText = "text"
	// ErrorFlagRead — флаг ошибки чтения (и признак недоступности устройства).
	ErrorFlagRead = "r"
	// ErrorFlagWrite — флаг ошибки записи.
	ErrorFlagWrite = "w"
	// devicesPrefix — корень устройств в MQTT.
	devicesPrefix = "/devices/"
	// tcpBroker — адрес брокера, если unix-сокета нет.
	tcpBroker = "tcp://localhost:1883"
)

// identifierPattern — допустимый идентификатор устройства или контрола (WB-STD-001).
var identifierPattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// DeviceMeta — содержимое /devices/<id>/meta.
type DeviceMeta struct {
	Driver string            `json:"driver"`
	Title  map[string]string `json:"title"`
}

// ControlMeta — содержимое /devices/<id>/controls/<control>/meta.
type ControlMeta struct {
	Type     string            `json:"type"`
	Title    map[string]string `json:"title,omitempty"`
	Order    int               `json:"order,omitempty"`
	Readonly bool              `json:"readonly,omitempty"`
}

// Control — контрол устройства.
type Control struct {
	ID   string
	Meta ControlMeta
	// Initial — значение при старте. Пустое значение не публикуется: пустое retained-сообщение
	// в MQTT удаляет значение, и wb-rules считает контрол удалённым, а следующее значение —
	// первым, на которое whenChanged не срабатывает.
	Initial string
	// Volatile — значение одноразовое (например, вызов команды): при (пере)подключении и после
	// неудачной публикации контрол возвращается к Initial, чтобы правило не получило вызов повторно.
	Volatile bool
}

// WriteHandler получает записи в /on записываемых контролов.
//
// Вызывается в отдельной горутине устройства, по одной записи и в порядке поступления,
// поэтому может вызывать SetValue/SetError. Пока обработчик занят, записи копятся в очереди
// (до 256); при переполненной очереди приём новых сообщений от брокера ждёт обработчик.
// Вызывать из обработчика Shutdown/Stop нельзя: они ждут его завершения.
type WriteHandler func(controlID string, payload []byte)

// DefaultBroker выбирает unix-сокет mosquitto, если он есть, иначе localhost:1883 — как сервисы WB.
func DefaultBroker(socketPath string) string {
	_, errStat := os.Stat(socketPath)
	if errStat == nil {
		return "unix://" + socketPath
	}

	return tcpBroker
}

// deviceMetaTopic возвращает топик meta устройства.
func deviceMetaTopic(device string) string {
	return devicesPrefix + device + "/meta"
}

// deviceErrorTopic возвращает топик ошибки устройства.
func deviceErrorTopic(device string) string {
	return deviceMetaTopic(device) + "/error"
}

// controlTopic возвращает топик значения контрола.
func controlTopic(device, control string) string {
	return devicesPrefix + device + "/controls/" + control
}

// controlMetaTopic возвращает топик meta контрола.
func controlMetaTopic(device, control string) string {
	return controlTopic(device, control) + "/meta"
}

// controlErrorTopic возвращает топик ошибки контрола.
func controlErrorTopic(device, control string) string {
	return controlMetaTopic(device, control) + "/error"
}

// controlOnTopic возвращает топик записи в контрол.
func controlOnTopic(device, control string) string {
	return controlTopic(device, control) + "/on"
}

// controlIDFromTopic извлекает ID контрола из любого топика под /devices/<device>/controls/<id>.
func controlIDFromTopic(device, topic string) (string, bool) {
	prefix := devicesPrefix + device + "/controls/"

	rest, ok := strings.CutPrefix(topic, prefix)
	if !ok || rest == "" {
		return "", false
	}

	id, _, _ := strings.Cut(rest, "/")

	return id, true
}
