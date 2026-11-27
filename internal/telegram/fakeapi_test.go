package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/telegram/mock"
)

// testToken - токен тестового Bot API.
const testToken = "123:secret"

// sentMessage - сообщение, принятое fixture Bot API.
type sentMessage struct {
	ChatID string
	Text   string
	At     time.Time
}

// fakeAPI хранит ответы и запросы fixture Bot API.
type fakeAPI struct {
	mu              sync.Mutex
	client          *mock.MockHTTPClient
	closed          bool
	sent            []sentMessage
	commands        map[string]string
	callbackAnswers map[string]string
	getMeCalls      int
	updates         []string
	sendStatuses    []string
	unauthorized    bool
	webhookDeleted  bool
	webhookStatus   string
	webhookCalls    int
	pollModes       []string
}

// newFakeAPI создаёт fixture Bot API над HTTP-моком.
func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()

	f := &fakeAPI{commands: map[string]string{}, callbackAnswers: map[string]string{}}
	f.client = mock.NewMockHTTPClient(gomock.NewController(t))
	f.client.EXPECT().Do(gomock.Any()).DoAndReturn(f.do).AnyTimes()

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
	case "getMe":
		f.mu.Lock()

		f.getMeCalls++

		f.mu.Unlock()

		writeResult(w, `{"id":123,"is_bot":true,"first_name":"Home","username":"MyHomeBot","active_usernames":["MyHomeBot","HomeAlias"],"can_join_groups":true,"can_read_all_group_messages":false,"supports_inline_queries":true,"inline_query_placeholder":"Search","has_topic_sidebar":true}`)
	case "answerCallbackQuery":
		f.mu.Lock()

		f.callbackAnswers[r.FormValue("callback_query_id")] = r.FormValue("text") + ":" + r.FormValue("show_alert")

		f.mu.Unlock()

		writeResult(w, "true")
	case "getUpdates":
		f.handleGetUpdates(w, r)
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

// handleDeleteWebhook отвечает на deleteWebhook заданным статусом ("" - успех, "429", "400").
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

// setWebhookStatus задаёт ответ на deleteWebhook: "" - успех, "429", "400".
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

// queuePollModes задаёт сетевую ошибку или ответ 429 для следующих getUpdates.
func (f *fakeAPI) queuePollModes(modes ...string) {
	f.mu.Lock()

	f.pollModes = append(f.pollModes, modes...)

	f.mu.Unlock()
}

// handleGetUpdates отдаёт накопленные апдейты или пустой список после короткой паузы.
func (f *fakeAPI) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()

	updates := f.updates
	f.updates = nil

	mode := ""
	if len(f.pollModes) > 0 {
		mode = f.pollModes[0]
		f.pollModes = f.pollModes[1:]
	}

	f.mu.Unlock()

	if mode == "429" {
		writeError(w, 429, "Too Many Requests", 1)

		return
	}

	if len(updates) == 0 {
		select {
		case <-time.After(50 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
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

// do отвечает на запрос HTTP-мока подготовленным результатом Bot API.
func (f *fakeAPI) do(request *http.Request) (*http.Response, error) {
	if request.Context().Err() != nil {
		return nil, request.Context().Err()
	}

	f.mu.Lock()

	closed := f.closed

	drop := strings.HasSuffix(request.URL.Path, "/getUpdates") && len(f.pollModes) > 0 && f.pollModes[0] == "drop"
	if drop {
		f.pollModes = f.pollModes[1:]
	}

	f.mu.Unlock()

	if closed || drop {
		return nil, fmt.Errorf("Post %s: %w", request.URL, errors.New("connection lost"))
	}

	recorder := httptest.NewRecorder()
	f.handle(recorder, request)

	if request.Context().Err() != nil {
		return nil, request.Context().Err()
	}

	return recorder.Result(), nil
}

// close заставляет HTTP-мок возвращать сетевую ошибку.
func (f *fakeAPI) close() {
	f.mu.Lock()

	f.closed = true

	f.mu.Unlock()
}

// newHTTPFixture создаёт HTTP-мок с проверкой запроса и записанным ответом.
func newHTTPFixture(t *testing.T, handler http.HandlerFunc) *mock.MockHTTPClient {
	t.Helper()

	client := mock.NewMockHTTPClient(gomock.NewController(t))
	client.EXPECT().Do(gomock.Any()).DoAndReturn(func(request *http.Request) (*http.Response, error) {
		if request.Context().Err() != nil {
			return nil, request.Context().Err()
		}

		recorder := httptest.NewRecorder()
		handler(recorder, request)

		return recorder.Result(), nil
	}).AnyTimes()

	return client
}
