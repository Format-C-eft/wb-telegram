package wbdevice

import (
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// observerSeq делает client id наблюдателей уникальными в пределах процесса.
var observerSeq atomic.Int64

// TestMain глушит журнал устройства, чтобы вывод тестов оставался чистым.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	os.Exit(m.Run())
}

// startBroker поднимает встроенный MQTT-брокер на случайном порту и возвращает его адрес.
func startBroker(t *testing.T) string {
	t.Helper()

	_, address := startBrokerServer(t)

	return address
}

// startBrokerServer поднимает встроенный MQTT-брокер с дополнительными хуками и возвращает сам сервер и его адрес.
func startBrokerServer(t *testing.T, hooks ...mqtt.Hook) (*mqtt.Server, string) {
	t.Helper()

	srv := mqtt.New(&mqtt.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	errHook := srv.AddHook(new(auth.AllowHook), nil)
	if errHook != nil {
		t.Fatalf("AddHook: %v", errHook)
	}

	errReleased := srv.AddHook(&releasedHook{srv: srv}, nil)
	if errReleased != nil {
		t.Fatalf("AddHook released: %v", errReleased)
	}

	for _, hook := range hooks {
		errExtra := srv.AddHook(hook, nil)
		if errExtra != nil {
			t.Fatalf("AddHook %s: %v", hook.ID(), errExtra)
		}
	}

	listener := listeners.NewTCP(listeners.Config{ID: "test", Address: "127.0.0.1:0"})

	errListener := srv.AddListener(listener)
	if errListener != nil {
		t.Fatalf("AddListener: %v", errListener)
	}

	errServe := srv.Serve()
	if errServe != nil {
		t.Fatalf("Serve: %v", errServe)
	}

	t.Cleanup(func() { closeBroker(srv) })

	return srv, "tcp://" + listener.Address()
}

// releasedHook задерживает CONNECT клиента, пока брокер не завершит отключение прежнего
// клиента с тем же client id.
//
// mochi убирает отключившегося клиента по client id, а не по экземпляру: если клиент
// переподключился (с тем же id) раньше, чем завершилась уборка прежнего соединения, уборка
// снимает подписки и запись уже нового клиента — он остаётся подключённым, но перестаёт
// получать сообщения. Настоящий брокер (mosquitto) так не делает; хук упорядочивает
// переподключение так, как его видит устройство на контроллере.
type releasedHook struct {
	mqtt.HookBase
	srv *mqtt.Server
}

// ID возвращает имя хука.
func (h *releasedHook) ID() string {
	return "wait-released"
}

// Provides сообщает, что хук обрабатывает OnConnect.
func (h *releasedHook) Provides(b byte) bool {
	return b == mqtt.OnConnect
}

// OnConnect ждёт (не дольше 5 секунд), пока брокер удалит прежнего клиента с тем же client id.
func (h *releasedHook) OnConnect(cl *mqtt.Client, _ packets.Packet) error {
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if _, ok := h.srv.Clients.Get(cl.ID); !ok {
			return nil
		}

		time.Sleep(5 * time.Millisecond)
	}

	return nil
}

// closeBroker останавливает брокер после того, как он отпустит всех клиентов.
//
// В mochi Close берёт RLock списка клиентов рекурсивно (GetByListener → Len); если в этот
// момент обработчик отключения клиента ждёт Lock на удаление, Close зависает навсегда.
// Поэтому сначала ждём, пока брокер удалит отключившихся клиентов.
func closeBroker(srv *mqtt.Server) {
	deadline := time.Now().Add(5 * time.Second)

	for srv.Clients.Len() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	_ = srv.Close()
}

// observer — тестовый MQTT-клиент, запоминающий последнее сообщение и число сообщений по каждому топику.
type observer struct {
	client   paho.Client
	mu       sync.Mutex
	messages map[string]string
	counts   map[string]int
}

// newObserver подключает наблюдателя к брокеру и подписывает на filter.
func newObserver(t *testing.T, broker, filter string) *observer {
	t.Helper()

	o := &observer{messages: map[string]string{}, counts: map[string]int{}}
	clientID := "observer-" + strconv.FormatInt(observerSeq.Add(1), 10) + "-" + t.Name()
	opts := paho.NewClientOptions().AddBroker(broker).SetClientID(clientID)
	o.client = paho.NewClient(opts)

	token := o.client.Connect()
	if !token.WaitTimeout(5*time.Second) || token.Error() != nil {
		t.Fatalf("observer connect: %v", token.Error())
	}

	sub := o.client.Subscribe(filter, 1, func(_ paho.Client, m paho.Message) {
		o.mu.Lock()
		o.messages[m.Topic()] = string(m.Payload())
		o.counts[m.Topic()]++
		o.mu.Unlock()
	})
	if !sub.WaitTimeout(5*time.Second) || sub.Error() != nil {
		t.Fatalf("observer subscribe: %v", sub.Error())
	}

	t.Cleanup(func() { o.client.Disconnect(100) })

	return o
}

// get возвращает последнее сообщение топика и признак его наличия.
func (o *observer) get(topic string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	value, ok := o.messages[topic]

	return value, ok
}

// count возвращает число сообщений, полученных по топику.
func (o *observer) count(topic string) int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.counts[topic]
}

// publish публикует сообщение от имени наблюдателя.
func (o *observer) publish(t *testing.T, topic, payload string, retained bool) {
	t.Helper()

	token := o.client.Publish(topic, 1, retained, payload)
	if !token.WaitTimeout(5*time.Second) || token.Error() != nil {
		t.Fatalf("publish %s: %v", topic, token.Error())
	}
}

// waitForRetained ждёт retained-сообщение topic, пришедшее наблюдателю при подписке.
//
// Встроенный брокер mochi отвечает SUBACK раньше, чем закончит обход retained-сообщений
// для wildcard-подписки, и этот обход гоняется (data race) с новыми retained-публикациями.
// Получение любого retained-сообщения значит, что обход завершён и публиковать безопасно.
func waitForRetained(t *testing.T, o *observer, topic string) {
	t.Helper()

	waitFor(t, "retained "+topic, func() bool {
		_, ok := o.get(topic)

		return ok
	})
}

// waitFor ждёт выполнения условия не дольше 10 секунд (с запасом на прогон под нагрузкой).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timeout waiting for %s", what)
}
