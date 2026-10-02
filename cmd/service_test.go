package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
)

// deviceMetaTopic — топик meta устройства бота.
const deviceMetaTopic = "/devices/telegram_bot/meta"

// clientSeq делает client id тестовых MQTT-клиентов уникальными в пределах процесса.
var clientSeq atomic.Int64

// startBroker поднимает встроенный MQTT-брокер на случайном порту и возвращает его адрес.
func startBroker(t *testing.T) string {
	t.Helper()

	srv := mqtt.New(&mqtt.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	errHook := srv.AddHook(new(auth.AllowHook), nil)
	if errHook != nil {
		t.Fatalf("AddHook: %v", errHook)
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

	return "tcp://" + listener.Address()
}

// closeBroker останавливает брокер после того, как он отпустит всех клиентов
// (Close в mochi может зависнуть, если в этот момент брокер ещё убирает отключившегося клиента).
func closeBroker(srv *mqtt.Server) {
	deadline := time.Now().Add(5 * time.Second)

	for srv.Clients.Len() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	_ = srv.Close()
}

// connectClient подключает тестовый MQTT-клиент с уникальным client id.
func connectClient(t *testing.T, broker, name string) paho.Client {
	t.Helper()

	clientID := name + "-" + strconv.FormatInt(clientSeq.Add(1), 10)
	client := paho.NewClient(paho.NewClientOptions().AddBroker(broker).SetClientID(clientID))

	token := client.Connect()
	if !token.WaitTimeout(5*time.Second) || token.Error() != nil {
		t.Fatalf("connect %s: %v", name, token.Error())
	}

	t.Cleanup(func() { client.Disconnect(100) })

	return client
}

// subscribe подписывает клиента на топик и ждёт подтверждения.
func subscribe(t *testing.T, client paho.Client, topic string, handler paho.MessageHandler) {
	t.Helper()

	token := client.Subscribe(topic, 1, handler)
	if !token.WaitTimeout(5*time.Second) || token.Error() != nil {
		t.Fatalf("subscribe %s: %v", topic, token.Error())
	}
}

// publish публикует сообщение и ждёт подтверждения.
func publish(t *testing.T, client paho.Client, topic, payload string, retained bool) {
	t.Helper()

	token := client.Publish(topic, 1, retained, payload)
	if !token.WaitTimeout(5*time.Second) || token.Error() != nil {
		t.Fatalf("publish %s: %v", topic, token.Error())
	}
}

// hasRetainedMeta сообщает, хранит ли брокер retained meta устройства бота: свежий клиент
// подписывается на топик и ждёт retained-сообщение.
func hasRetainedMeta(t *testing.T, broker string) bool {
	t.Helper()

	client := connectClient(t, broker, "check")
	received := make(chan struct{}, 1)

	subscribe(t, client, deviceMetaTopic, func(_ paho.Client, m paho.Message) {
		if m.Retained() && len(m.Payload()) > 0 {
			select {
			case received <- struct{}{}:
			default:
			}
		}
	})

	select {
	case <-received:
		return true
	case <-time.After(300 * time.Millisecond):
		return false
	}
}

// fakeTelegram — минимальный поддельный Bot API для тестов сервиса.
type fakeTelegram struct {
	unauthorized bool
	failSend     bool
	sendAttempts atomic.Int64

	mu      sync.Mutex
	updates []string
	sent    []string
}

// ServeHTTP отвечает на getUpdates, sendMessage и прочие методы; при unauthorized — 401 на всё,
// при failSend — 500 на sendMessage.
func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseMultipartForm(1 << 20) //nolint:gosec // поддельный сервер в тестах, размер формы мал

	if f.unauthorized {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)

		return
	}

	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

	if method == "getUpdates" {
		f.mu.Lock()

		updates := f.updates
		f.updates = nil

		f.mu.Unlock()

		if len(updates) == 0 {
			time.Sleep(50 * time.Millisecond)
		}

		_, _ = fmt.Fprintf(w, `{"ok":true,"result":[%s]}`, strings.Join(updates, ","))

		return
	}

	if method != "sendMessage" {
		_, _ = fmt.Fprint(w, `{"ok":true,"result":true}`)

		return
	}

	f.sendAttempts.Add(1)

	if f.failSend {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`)

		return
	}

	f.mu.Lock()

	f.sent = append(f.sent, r.FormValue("chat_id")+":"+r.FormValue("text"))

	f.mu.Unlock()

	_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1,"type":"private"}}}`)
}

// pushUpdate ставит текстовое сообщение от chatID в ответ getUpdates.
func (f *fakeTelegram) pushUpdate(updateID, chatID int64, text string) {
	update := fmt.Sprintf(`{"update_id":%d,"message":{"message_id":1,"date":%d,"chat":{"id":%d,"type":"private"},"text":%q}}`,
		updateID, time.Now().Unix(), chatID, text)

	f.mu.Lock()

	f.updates = append(f.updates, update)

	f.mu.Unlock()
}

// sentMessages возвращает копию отправленных сообщений в виде "chat_id:текст".
func (f *fakeTelegram) sentMessages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.sent)
}

// writeFiles пишет конфиг и, если token не пуст, файл токена; возвращает их пути.
func writeFiles(t *testing.T, config, token string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "wb-telegram.conf")
	tokenPath := filepath.Join(dir, "token")

	errConfig := os.WriteFile(configPath, []byte(config), 0o600)
	if errConfig != nil {
		t.Fatalf("write config: %v", errConfig)
	}

	if token != "" {
		errToken := os.WriteFile(tokenPath, []byte(token), 0o600)
		if errToken != nil {
			t.Fatalf("write token: %v", errToken)
		}
	}

	return configPath, tokenPath
}

// TestRunServiceExitCodes проверяет коды выхода без запуска бота и уборку устройства из MQTT,
// когда бот выключен или токен не задан.
func TestRunServiceExitCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		config    string
		token     string
		wantCode  int
		wantPurge bool
	}{
		{name: "disabled", config: `{"enabled": false}`, token: "1:a", wantCode: 0, wantPurge: true},
		{name: "no token", config: `{"enabled": true}`, wantCode: 0, wantPurge: true},
		{name: "broken config", config: `{"enabled": tru`, token: "1:a", wantCode: 1},
		{name: "invalid rules", config: `{"enabled": true, "reply_timeout_s": 1000}`, token: "1:a", wantCode: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			broker := startBroker(t)
			seed := connectClient(t, broker, "seed")

			publish(t, seed, deviceMetaTopic, `{"driver":"wb-telegram"}`, true)

			configPath, tokenPath := writeFiles(t, tt.config, tt.token)

			code := runService(context.Background(), serviceSettings{configPath: configPath, tokenPath: tokenPath, broker: broker})
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d", code, tt.wantCode)
			}

			if retained := hasRetainedMeta(t, broker); retained == tt.wantPurge {
				t.Fatalf("device meta retained = %v, want %v", retained, !tt.wantPurge)
			}
		})
	}
}

// TestRunServiceUnauthorized проверяет, что отклонённый Telegram токен останавливает сервис
// с кодом 0 и убирает устройство из MQTT.
func TestRunServiceUnauthorized(t *testing.T) {
	t.Parallel()

	broker := startBroker(t)
	seed := connectClient(t, broker, "seed")

	publish(t, seed, deviceMetaTopic, `{"driver":"wb-telegram"}`, true)

	api := httptest.NewServer(&fakeTelegram{unauthorized: true})
	t.Cleanup(api.Close)

	configPath, tokenPath := writeFiles(t, `{"enabled": true, "users": [{"name": "Я", "chat_id": 100}]}`, "123:secret")
	done := make(chan int, 1)

	go func() {
		done <- runService(context.Background(), serviceSettings{configPath: configPath, tokenPath: tokenPath, broker: broker, apiURL: api.URL})
	}()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("service did not stop after 401")
	}

	if hasRetainedMeta(t, broker) {
		t.Fatal("device meta is still retained after 401")
	}
}

// runningService — сервис, запущенный в фоне, и клиент «правила» на том же брокере.
type runningService struct {
	rule   paho.Client
	calls  chan string
	cancel context.CancelFunc
	done   chan int

	mu          sync.Mutex
	deviceError *string
}

// errorFlag возвращает последнее значение meta/error устройства и признак, что оно приходило.
func (r *runningService) errorFlag() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.deviceError == nil {
		return "", false
	}

	return *r.deviceError, true
}

// startService запускает runService с поддельным Bot API и ждёт, пока устройство станет готовым:
// пустой meta/error публикуется последним шагом, когда записи в /on уже принимаются.
func startService(t *testing.T, tg *fakeTelegram, config string) *runningService {
	t.Helper()

	broker := startBroker(t)
	api := httptest.NewServer(tg)
	t.Cleanup(api.Close)

	configPath, tokenPath := writeFiles(t, config, "123:secret")
	svc := &runningService{rule: connectClient(t, broker, "rule"), calls: make(chan string, 16), done: make(chan int, 1)}

	subscribe(t, svc.rule, "/devices/telegram_bot/controls/cmd_gate", func(_ paho.Client, m paho.Message) {
		if len(m.Payload()) > 0 {
			svc.calls <- string(m.Payload())
		}
	})
	subscribe(t, svc.rule, "/devices/telegram_bot/meta/error", func(_ paho.Client, m paho.Message) {
		value := string(m.Payload())

		svc.mu.Lock()

		svc.deviceError = &value

		svc.mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	svc.cancel = cancel
	t.Cleanup(cancel)

	go func() {
		svc.done <- runService(ctx, serviceSettings{configPath: configPath, tokenPath: tokenPath, broker: broker, apiURL: api.URL})
	}()

	waitUntil(t, "device ready", func() bool {
		value, ok := svc.errorFlag()

		return ok && value == ""
	})

	return svc
}

// stopAndWait отменяет контекст сервиса, проверяет код выхода 0 и meta/error = r после остановки.
func (r *runningService) stopAndWait(t *testing.T, limit time.Duration) {
	t.Helper()

	r.cancel()

	select {
	case code := <-r.done:
		if code != 0 {
			t.Fatalf("exit code = %d", code)
		}
	case <-time.After(limit):
		t.Fatal("service did not stop")
	}

	waitUntil(t, "device marked offline", func() bool {
		value, _ := r.errorFlag()

		return value == "r"
	})
}

// TestRunServiceEndToEnd проверяет путь команда → MQTT → ответ правила → Telegram и штатную остановку.
func TestRunServiceEndToEnd(t *testing.T) {
	t.Parallel()

	tg := &fakeTelegram{}
	svc := startService(t, tg,
		`{"enabled": true, "reply_timeout_s": 10, "users": [{"name": "Я", "chat_id": 100, "notifications": true}], "commands": [{"name": "gate", "description": "Ворота"}]}`)

	tg.pushUpdate(42, 100, "/gate 22")

	var payload string

	select {
	case payload = <-svc.calls:
	case <-time.After(10 * time.Second):
		t.Fatalf("command was not published to MQTT, sent = %v", tg.sentMessages())
	}

	var call struct {
		ID   string `json:"id"`
		User string `json:"user"`
		Args string `json:"args"`
	}

	errUnmarshal := json.Unmarshal([]byte(payload), &call)
	if errUnmarshal != nil || call.ID != "42" || call.User != "Я" || call.Args != "22" {
		t.Fatalf("call = %s, err = %v", payload, errUnmarshal)
	}

	publish(t, svc.rule, "/devices/telegram_bot/controls/send/on", `{"text":"Открываю","reply_to":"42"}`, false)

	waitUntil(t, "reply delivered", func() bool {
		return slices.Contains(tg.sentMessages(), "100:Открываю")
	})

	if sent := tg.sentMessages(); len(sent) != 1 {
		t.Fatalf("sent = %v, want only the reply", sent)
	}

	svc.stopAndWait(t, 10*time.Second)
}

// TestRunServiceStopWithTelegramDown проверяет остановку, когда Telegram недоступен, а в очереди есть
// сообщение: досылка ограничена 5 с, затем устройство всё равно помечается r и код выхода 0.
func TestRunServiceStopWithTelegramDown(t *testing.T) {
	t.Parallel()

	tg := &fakeTelegram{failSend: true}
	svc := startService(t, tg, `{"enabled": true, "users": [{"name": "Я", "chat_id": 100, "notifications": true}]}`)

	publish(t, svc.rule, "/devices/telegram_bot/controls/send/on", "тревога", false)

	waitUntil(t, "send attempted", func() bool { return tg.sendAttempts.Load() > 0 })

	svc.stopAndWait(t, 15*time.Second)
}

// waitUntil ждёт выполнения условия не дольше 10 секунд.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("timeout waiting for %s", what)
}
