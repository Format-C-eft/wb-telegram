package telegram

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testToken — токен, который понимает поддельный сервер.
const testToken = "123:secret"

// sentMessage — сообщение, полученное поддельным сервером.
type sentMessage struct {
	ChatID string
	Text   string
	At     time.Time
}

// fakeAPI — поддельный Bot API на httptest.Server.
type fakeAPI struct {
	mu             sync.Mutex
	server         *httptest.Server
	sent           []sentMessage
	commands       map[string]string
	updates        []string
	sendStatuses   []string
	unauthorized   bool
	webhookDeleted bool
	webhookStatus  string
	webhookCalls   int
}

// newFakeAPI запускает поддельный Bot API.
func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()

	f := &fakeAPI{commands: map[string]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)

	return f
}

// queueUpdate добавляет апдейт с текстовым сообщением в выдачу getUpdates.
func (f *fakeAPI) queueUpdate(updateID, chatID int64, text string, date time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.updates = append(f.updates, fmt.Sprintf(
		`{"update_id":%d,"message":{"message_id":1,"date":%d,"chat":{"id":%d,"type":"private"},"text":%q}}`,
		updateID, date.Unix(), chatID, text))
}

// queueRawUpdate добавляет произвольный апдейт в выдачу getUpdates.
func (f *fakeAPI) queueRawUpdate(raw string) {
	f.mu.Lock()

	f.updates = append(f.updates, raw)

	f.mu.Unlock()
}

// queueSendStatus задаёт ответы на следующие вызовы sendMessage: "ok", "429", "401", "403", "500".
func (f *fakeAPI) queueSendStatus(statuses ...string) {
	f.mu.Lock()

	f.sendStatuses = append(f.sendStatuses, statuses...)

	f.mu.Unlock()
}

// setUnauthorized заставляет все методы отвечать 401.
func (f *fakeAPI) setUnauthorized() {
	f.mu.Lock()

	f.unauthorized = true

	f.mu.Unlock()
}

// sentMessages возвращает копию полученных сообщений.
func (f *fakeAPI) sentMessages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]sentMessage(nil), f.sent...)
}

// handle разбирает вызов Bot API и отвечает как Telegram.
func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+testToken+"/")
	if !ok {
		writeError(w, 404, "Not Found", 0)

		return
	}

	_ = r.ParseMultipartForm(1 << 20) //nolint:gosec // поддельный сервер в тестах, размер формы мал

	f.mu.Lock()
	unauthorized := f.unauthorized
	f.mu.Unlock()

	if unauthorized {
		writeError(w, 401, "Unauthorized", 0)

		return
	}

	switch method {
	case "getUpdates":
		f.handleGetUpdates(w)
	case "deleteWebhook":
		f.handleDeleteWebhook(w)
	case "setMyCommands":
		f.mu.Lock()

		f.commands[r.FormValue("scope")] = r.FormValue("commands")

		f.mu.Unlock()

		writeResult(w, "true")
	case "sendMessage":
		f.handleSendMessage(w, r)
	default:
		writeError(w, 404, "Not Found", 0)
	}
}

// handleDeleteWebhook отвечает на deleteWebhook заданным статусом ("" — успех, "429", "400").
func (f *fakeAPI) handleDeleteWebhook(w http.ResponseWriter) {
	f.mu.Lock()

	f.webhookCalls++
	status := f.webhookStatus

	if status == "" {
		f.webhookDeleted = true
	}

	f.mu.Unlock()

	switch status {
	case "":
		writeResult(w, "true")
	case "429":
		writeError(w, 429, "Too Many Requests", 1)
	default:
		writeError(w, 400, "Bad Request", 0)
	}
}

// setWebhookStatus задаёт ответ на deleteWebhook: "" — успех, "429", "400".
func (f *fakeAPI) setWebhookStatus(status string) {
	f.mu.Lock()

	f.webhookStatus = status

	f.mu.Unlock()
}

// webhookCallCount возвращает число вызовов deleteWebhook.
func (f *fakeAPI) webhookCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.webhookCalls
}

// handleGetUpdates отдаёт накопленные апдейты или пустой список после короткой паузы.
func (f *fakeAPI) handleGetUpdates(w http.ResponseWriter) {
	f.mu.Lock()

	updates := f.updates
	f.updates = nil

	f.mu.Unlock()

	if len(updates) == 0 {
		time.Sleep(50 * time.Millisecond)
	}

	writeResult(w, "["+strings.Join(updates, ",")+"]")
}

// handleSendMessage запоминает сообщение или отвечает заданной ошибкой.
func (f *fakeAPI) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()

	status := "ok"
	if len(f.sendStatuses) > 0 {
		status = f.sendStatuses[0]
		f.sendStatuses = f.sendStatuses[1:]
	}

	if status == "ok" {
		f.sent = append(f.sent, sentMessage{ChatID: r.FormValue("chat_id"), Text: r.FormValue("text"), At: time.Now()})
	}

	f.mu.Unlock()

	switch status {
	case "ok":
		writeResult(w, `{"message_id":1,"date":0,"chat":{"id":1,"type":"private"}}`)
	case "429":
		writeError(w, 429, "Too Many Requests", 1)
	case "401":
		writeError(w, 401, "Unauthorized", 0)
	case "403":
		writeError(w, 403, "Forbidden: bot was blocked by the user", 0)
	default:
		writeError(w, 500, "Internal Server Error", 0)
	}
}

// writeResult пишет успешный ответ Bot API.
func writeResult(w http.ResponseWriter, result string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"result":%s}`, result)
}

// writeError пишет ответ Bot API с ошибкой.
func writeError(w http.ResponseWriter, code int, description string, retryAfter int) {
	body := map[string]any{"ok": false, "error_code": code, "description": description}
	if retryAfter > 0 {
		body["parameters"] = map[string]any{"retry_after": retryAfter}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// waitFor ждёт выполнения условия не дольше 5 секунд.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timeout waiting for %s", what)
}
