package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// interactionRequest хранит фактический запрос Telegram.
type interactionRequest struct {
	method   string
	fields   map[string]string
	photo    []byte
	filename string
}

// interactionTelegram обслуживает polling и сохраняет сообщения, фото и ACK.
type interactionTelegram struct {
	mu       sync.Mutex
	updates  []string
	requests chan interactionRequest
}

// ServeHTTP обслуживает локальный Bot API сквозного теста.
func (f *interactionTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if method == "getUpdates" {
		f.mu.Lock()
		updates := f.updates
		f.updates = nil
		f.mu.Unlock()

		if len(updates) == 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}

		_, _ = fmt.Fprintf(w, `{"ok":true,"result":[%s]}`, strings.Join(updates, ","))

		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)

	errForm := r.ParseMultipartForm(4 << 20) //nolint:gosec // MaxBytesReader ограничивает тело запроса
	if errForm != nil && !strings.Contains(errForm.Error(), "not multipart") {
		w.WriteHeader(http.StatusBadRequest)

		return
	}

	request := interactionRequest{method: method, fields: make(map[string]string)}
	for key := range r.Form {
		request.fields[key] = r.FormValue(key)
	}

	if method == "sendPhoto" && !readInteractionPhoto(r, &request) {
		w.WriteHeader(http.StatusBadRequest)

		return
	}

	select {
	case f.requests <- request:
	case <-r.Context().Done():
		return
	}

	switch method {
	case "sendMessage", "sendPhoto", "sendWidget", "editMessageReplyMarkup":
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":77,"date":1,"chat":{"id":100,"type":"private"}}}`)

		return
	}

	_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
}

// readInteractionPhoto сохраняет загруженное изображение.
func readInteractionPhoto(r *http.Request, request *interactionRequest) bool {
	file, header, errFile := r.FormFile("photo")
	if errFile != nil {
		return false
	}
	defer file.Close()

	photo, errRead := io.ReadAll(file)
	if errRead != nil {
		return false
	}

	request.photo = photo
	request.filename = header.Filename

	return true
}

// push добавляет обновление в реальный путь getUpdates.
func (f *interactionTelegram) push(update string) {
	f.mu.Lock()
	f.updates = append(f.updates, update)
	f.mu.Unlock()
}

// awaitRequest ждёт фактический запрос указанного метода и текста.
func (f *interactionTelegram) awaitRequest(t *testing.T, method, text string) interactionRequest {
	t.Helper()

	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()

	for {
		select {
		case request := <-f.requests:
			if request.method == method && (text == "" || request.fields["text"] == text) {
				return request
			}
		case <-timeout.C:
			t.Fatalf("no Telegram request %s %q", method, text)

			return interactionRequest{}
		}
	}
}

// startInteractionService запускает настоящий сервис на локальных MQTT и HTTP.
func startInteractionService(t *testing.T) (*interactionTelegram, *runningService, string) {
	t.Helper()

	broker := startBroker(t)
	tg := &interactionTelegram{requests: make(chan interactionRequest, 64)}
	api := httptest.NewServer(tg)
	t.Cleanup(api.Close)

	configPath, tokenPath := writeFiles(t, `{"enabled":true,"reply_timeout_s":10,"users":[{"name":"Tester","chat_id":100,"notifications":true}],"commands":[{"name":"gate","description":"Gate"},{"name":"boiler_status","description":"Boiler status"}]}`, "123:secret")
	svc := &runningService{rule: connectClient(t, broker, "interaction-rule"), calls: make(chan string, 16), done: make(chan int, 1)}

	for _, command := range []string{"gate", "boiler_status"} {
		subscribe(t, svc.rule, "/devices/telegram_bot/controls/cmd_"+command, func(_ paho.Client, m paho.Message) {
			if payload := string(m.Payload()); payload != "" && payload != "{}" {
				svc.calls <- payload
			}
		})
	}

	subscribe(t, svc.rule, "/devices/telegram_bot/meta/error", func(_ paho.Client, m paho.Message) {
		value := string(m.Payload())

		svc.mu.Lock()
		svc.deviceError = &value
		svc.mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())

	svc.cancel = cancel
	go func() {
		svc.done <- runService(ctx, serviceSettings{configPath: configPath, tokenPath: tokenPath, broker: broker, apiURL: api.URL})
	}()

	t.Cleanup(func() { svc.stopAndWait(t, 10*time.Second) })

	waitUntil(t, "interaction service ready", func() bool {
		value, ok := svc.errorFlag()

		return ok && value == ""
	})
	tg.awaitRequest(t, "sendMessage", "")

	return tg, svc, broker
}

// awaitInteractionCall получает вызов правила из настоящего MQTT.
func awaitInteractionCall(t *testing.T, svc *runningService) map[string]string {
	t.Helper()

	select {
	case payload := <-svc.calls:
		var call map[string]json.RawMessage

		errJSON := json.Unmarshal([]byte(payload), &call)
		if errJSON != nil {
			t.Fatal(errJSON)
		}

		fields := make(map[string]string)

		for _, key := range []string{"id", "user", "args"} {
			var value string

			errField := json.Unmarshal(call[key], &value)
			if errField != nil {
				t.Fatal(errField)
			}

			fields[key] = value
		}

		fields["can_reply_ephemeral"] = string(call["can_reply_ephemeral"])

		return fields
	case <-time.After(10 * time.Second):
		t.Fatal("no MQTT command")

		return nil
	}
}

// interactionCallbackToken проверяет клавиатуру и извлекает непрозрачный токен.
func interactionCallbackToken(t *testing.T, raw string) string {
	t.Helper()

	var markup struct {
		Rows [][]struct {
			Data string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}

	errMarkup := json.Unmarshal([]byte(raw), &markup)
	if errMarkup != nil {
		t.Fatal(errMarkup)
	}

	if len(markup.Rows) != 1 || len(markup.Rows[0]) != 1 || markup.Rows[0][0].Data == "" {
		t.Fatal("missing callback token")
	}

	return markup.Rows[0][0].Data
}

// TestRunServiceInlineInteraction проверяет команду, HTML-кнопку, ACK и однократный вызов.
func TestRunServiceInlineInteraction(t *testing.T) {
	t.Parallel()

	tg, svc, _ := startInteractionService(t)
	tg.push(fmt.Sprintf(`{"update_id":42,"message":{"message_id":1,"date":%d,"chat":{"id":100,"type":"private"},"text":"/gate choose"}}`, time.Now().Unix()))

	call := awaitInteractionCall(t, svc)
	if call["id"] != "42" || call["args"] != "choose" || call["user"] != "Tester" {
		t.Fatalf("command: %v", call)
	}

	publish(t, svc.rule, "/devices/telegram_bot/controls/send/on", `{"reply_to":"42","text":"<b>Open?</b>","parse_mode":"HTML","buttons":[[{"text":"Yes","command":"gate","args":"open"}]]}`, false)

	sent := tg.awaitRequest(t, "sendMessage", "<b>Open?</b>")
	if sent.fields["chat_id"] != "100" || sent.fields["parse_mode"] != "HTML" {
		t.Fatalf("message: %+v", sent.fields)
	}

	token := interactionCallbackToken(t, sent.fields["reply_markup"])

	callback := fmt.Sprintf(`{"update_id":43,"callback_query":{"id":"query-43","from":{"id":100},"message":{"message_id":77,"date":1,"chat":{"id":100,"type":"private"}},"data":%q}}`, token)
	tg.push(callback)

	ack := tg.awaitRequest(t, "answerCallbackQuery", "")
	if ack.fields["callback_query_id"] != "query-43" {
		t.Fatal("wrong callback ACK")
	}

	selected := awaitInteractionCall(t, svc)
	if selected["args"] != "open" || selected["id"] != "43" || selected["can_reply_ephemeral"] != "true" {
		t.Fatalf("callback command: %v", selected)
	}

	publish(t, svc.rule, "/devices/telegram_bot/controls/send/on", `{"reply_to":"43","text":"Temporary result","ephemeral":true}`, false)
	ephemeral := tg.awaitRequest(t, "sendMessage", "Temporary result")

	var ephemeralParameters struct {
		CallbackQueryID string `json:"callback_query_id"`
	}

	errEphemeral := json.Unmarshal([]byte(ephemeral.fields["ephemeral_message_parameters"]), &ephemeralParameters)
	if errEphemeral != nil {
		t.Fatal(errEphemeral)
	}

	if _, hasChat := ephemeral.fields["chat_id"]; hasChat || ephemeralParameters.CallbackQueryID != "query-43" {
		t.Fatalf("ephemeral message parameters: %+v", ephemeral.fields)
	}

	tg.push(callback)
	tg.awaitRequest(t, "answerCallbackQuery", "")

	select {
	case duplicate := <-svc.calls:
		t.Fatalf("duplicate MQTT command: %s", duplicate)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestRunServiceWidgetInteraction проверяет обновление данных виджета через MQTT.
func TestRunServiceWidgetInteraction(t *testing.T) {
	t.Parallel()

	tg, svc, _ := startInteractionService(t)
	tg.push(fmt.Sprintf(`{"update_id":50,"message":{"message_id":1,"date":%d,"chat":{"id":100,"type":"private"},"text":"/boiler_status"}}`, time.Now().Unix()))

	call := awaitInteractionCall(t, svc)
	if call["id"] != "50" || call["user"] != "Tester" {
		t.Fatalf("widget command: %v", call)
	}

	publish(t, svc.rule, "/devices/telegram_bot/controls/send/on", `{"reply_to":"50","boiler_widget":{"temperature":"50 °C","status":"Работает"}}`, false)

	sent := tg.awaitRequest(t, "sendWidget", "")
	if sent.fields["chat_id"] != "100" || sent.fields["allow_queries"] != "true" || !strings.Contains(sent.fields["chat_data"], "50 °C") {
		t.Fatalf("widget message: %v", sent.fields["chat_data"])
	}

	tg.push(`{"update_id":51,"widget_query":{"id":"widget-51","from":{"id":100},"chat":{"id":100,"type":"private"},"message_id":77,"type":{"type":"data"},"data":"refresh"}}`)

	refresh := awaitInteractionCall(t, svc)
	if refresh["id"] != "51" || refresh["user"] != "Tester" {
		t.Fatalf("widget command: %v", refresh)
	}

	publish(t, svc.rule, "/devices/telegram_bot/controls/send/on", `{"reply_to":"51","boiler_widget":{"temperature":"54 °C","status":"Работает"}}`, false)
	answer := tg.awaitRequest(t, "answerWidgetQuery", "")

	var widgetData struct {
		Temperature string `json:"temperature"`
		Status      string `json:"status"`
	}

	errData := json.Unmarshal([]byte(answer.fields["data"]), &widgetData)
	if errData != nil {
		t.Fatal(errData)
	}

	if answer.fields["widget_query_id"] != "widget-51" || widgetData.Temperature != "54 °C" || widgetData.Status != "Работает" {
		t.Fatalf("widget answer: %+v, data: %+v", answer.fields, widgetData)
	}

	edit := tg.awaitRequest(t, "editMessageWidgetData", "")
	if edit.fields["chat_id"] != "100" || edit.fields["message_id"] != "77" || edit.fields["chat_data"] != answer.fields["data"] {
		t.Fatalf("widget edit: %+v", edit.fields)
	}
}

// TestRunServiceChartInteraction проверяет RPC истории, независимый текст и multipart PNG.
func TestRunServiceChartInteraction(t *testing.T) {
	t.Parallel()

	tg, svc, _ := startInteractionService(t)
	requests := make(chan struct {
		topic, id string
		timestamp float64
	}, 1)

	subscribe(t, svc.rule, "/rpc/v1/db_logger/history/get_values/+", func(_ paho.Client, m paho.Message) {
		if m.Retained() {
			t.Error("retained history request")
		}

		var query struct {
			ID     string `json:"id"`
			Params struct {
				Timestamp struct {
					GT float64 `json:"gt"`
					LT float64 `json:"lt"`
				} `json:"timestamp"`
				Channels [][2]string `json:"channels"`
			} `json:"params"`
		}

		errJSON := json.Unmarshal(m.Payload(), &query)
		if errJSON != nil {
			t.Error(errJSON)

			return
		}

		if len(query.Params.Channels) != 1 || query.Params.Channels[0] != [2]string{"sensor", "temperature"} {
			t.Error("wrong history channel")
		}

		requests <- struct {
			topic, id string
			timestamp float64
		}{m.Topic(), query.ID, (query.Params.Timestamp.GT + query.Params.Timestamp.LT) / 2}
	})
	publish(t, svc.rule, "/devices/telegram_bot/controls/send/on", `{"id":"chart-1","to":["Tester"],"chart":{"title":"Room","period_s":3600,"channels":[{"device":"sensor","control":"temperature","label":"Room","unit":"C"}]}}`, false)

	var query struct {
		topic, id string
		timestamp float64
	}
	select {
	case query = <-requests:
	case <-time.After(5 * time.Second):
		t.Fatal("no history RPC")
	}

	publish(t, svc.rule, "/devices/telegram_bot/controls/send/on", `{"to":["Tester"],"text":"Text during RPC"}`, false)
	tg.awaitRequest(t, "sendMessage", "Text during RPC")
	publish(t, svc.rule, query.topic+"/reply", fmt.Sprintf(`{"id":%q,"result":{"values":[{"c":1,"i":1,"t":%.3f,"v":"22.5"}],"has_more":false}}`, query.id, query.timestamp), false)

	photo := tg.awaitRequest(t, "sendPhoto", "")
	if photo.fields["chat_id"] != "100" || !strings.Contains(photo.fields["caption"], "Room") || photo.filename != "chart.png" {
		t.Fatalf("photo metadata: %+v", photo.fields)
	}

	image, errPNG := png.Decode(bytes.NewReader(photo.photo))
	if errPNG != nil {
		t.Fatal(errPNG)
	}

	if image.Bounds().Dx() != 1000 || image.Bounds().Dy() != 600 {
		t.Fatalf("PNG dimensions: %v", image.Bounds())
	}
}
