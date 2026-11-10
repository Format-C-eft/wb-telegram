package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

// TestDeliveryOwnership проверяет копирование и байтовый бюджет очереди.
func TestDeliveryOwnership(t *testing.T) {
	t.Parallel()

	client, errNew := New(testToken, func(context.Context, Message) {})
	if errNew != nil {
		t.Fatal(errNew)
	}

	original := Delivery{Kind: DeliveryText, Text: "text", Keyboard: InlineKeyboard{{{Text: "button", Data: "opaque"}}}}

	errSend := client.SendDelivery(1, original)
	if errSend != nil {
		t.Fatal(errSend)
	}

	original.Keyboard[0][0].Data = "modified"

	item, ok := client.queue.pop(context.Background())
	if !ok || item.delivery.Keyboard[0][0].Data != "opaque" {
		t.Fatal("keyboard aliases caller")
	}

	png := bytes.Repeat([]byte{1}, MaxPNGBytes)
	for range 8 {
		errSend = client.SendDelivery(1, Delivery{Kind: DeliveryPhoto, PNG: png})
		if errSend != nil {
			t.Fatal(errSend)
		}
	}

	png[0] = 2

	errSend = client.SendDelivery(1, Delivery{Kind: DeliveryPhoto, PNG: make([]byte, MaxPNGBytes+1)})
	if !errors.Is(errSend, ErrInvalidArgument) || client.queue.len() != 8 {
		t.Fatal("oversized image displaced queue")
	}

	errSend = client.SendDelivery(1, Delivery{Kind: DeliveryPhoto, PNG: png})
	if !errors.Is(errSend, ErrQueueFull) || client.Stats().Dropped != 1 {
		t.Fatalf("overflow: %v %+v", errSend, client.Stats())
	}

	item, ok = client.queue.pop(context.Background())
	if !ok || item.delivery.PNG[0] != 1 {
		t.Fatal("PNG aliases caller")
	}

	if client.queue.bytes != 7*MaxPNGBytes {
		t.Fatal("wrong byte accounting")
	}
}

// TestDeliveryUTF16 проверяет пределы UTF-16 и отказ от обрезания разметки.
func TestDeliveryUTF16(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("😀", 2049)

	truncated := truncate(text)
	if utf16Length(truncated) > 4096 || !strings.HasSuffix(truncated, "…") {
		t.Fatal("invalid UTF16 truncation")
	}

	for _, delivery := range []Delivery{
		{Kind: DeliveryText, Text: text, ParseMode: ParseModeHTML},
		{Kind: DeliveryText, Text: text, ParseMode: ParseModeMarkdownV2},
		{Kind: DeliveryPhoto, PNG: []byte{1}, Caption: strings.Repeat("😀", 513)},
		{Kind: DeliveryPhoto, PNG: []byte{1}, Keyboard: InlineKeyboard{{{Text: "x", Data: "x"}}}},
	} {
		if !errors.Is(delivery.validate(), ErrInvalidArgument) {
			t.Fatalf("accepted %+v", delivery.Kind)
		}
	}
}

// TestPhotoRetryReader проверяет полный multipart при повторной отправке.
func TestPhotoRetryReader(t *testing.T) {
	t.Parallel()

	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 100)...)

	var (
		mu      sync.Mutex
		uploads [][]byte
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)

		errForm := r.ParseMultipartForm(4 << 20) //nolint:gosec // тело ограничено MaxBytesReader
		if errForm != nil {
			t.Error(errForm)
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		file, header, errFile := r.FormFile("photo")
		if errFile != nil {
			t.Error(errFile)
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		defer file.Close()

		data, errRead := io.ReadAll(file)
		if errRead != nil {
			t.Error(errRead)

			return
		}

		if header.Filename != "chart.png" || r.FormValue("caption") != "<b>chart</b>" || r.FormValue("parse_mode") != "HTML" {
			t.Error("wrong photo metadata")
		}

		mu.Lock()

		uploads = append(uploads, data)
		count := len(uploads)
		mu.Unlock()

		if count == 1 {
			writeError(w, http.StatusInternalServerError, "temporary", 0)

			return
		}

		_, errWrite := io.WriteString(w, `{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":1,"type":"private"}}}`)
		if errWrite != nil {
			t.Error(errWrite)
		}
	}))
	defer server.Close()

	client, errNew := New(testToken, func(context.Context, Message) {}, WithServerURL(server.URL), WithRateLimits(0, 0), withSleep(noSleep))
	if errNew != nil {
		t.Fatal(errNew)
	}

	delivery := Delivery{Kind: DeliveryPhoto, PNG: png, Caption: "<b>chart</b>", ParseMode: ParseModeHTML}
	if !client.deliver(context.Background(), outgoing{chatID: 1, delivery: delivery}) {
		t.Fatal("delivery canceled")
	}

	mu.Lock()
	defer mu.Unlock()

	if len(uploads) != 2 || !bytes.Equal(uploads[0], png) || !bytes.Equal(uploads[1], png) || client.Stats().Sent != 1 {
		t.Fatal("incomplete retry upload")
	}
}

// TestCallbackUnion проверяет доступные, недоступные и inline нажатия.
func TestCallbackUnion(t *testing.T) {
	t.Parallel()

	for _, fixture := range []struct {
		message    string
		accessible bool
		chat       int64
	}{
		{`"message":{"message_id":3,"date":100,"chat":{"id":7,"type":"private"}},`, true, 7},
		{`"message":{"message_id":3,"date":0,"chat":{"id":7,"type":"private"}},`, false, 7},
		{`"inline_message_id":"inline",`, false, 0},
		{``, false, 0},
	} {
		update := &models.Update{}

		errDecode := json.Unmarshal([]byte(`{"update_id":8,"callback_query":{`+fixture.message+`"id":"query","from":{"id":9},"data":"token"}}`), update)
		if errDecode != nil {
			t.Fatal(errDecode)
		}

		callback := callbackFromUpdate(update)
		if callback.Accessible != fixture.accessible || callback.ChatID != fixture.chat || callback.UpdateID != 8 || callback.FromID != 9 {
			t.Fatalf("callback: %+v", callback)
		}
	}
}

// TestControlRequests проверяет отдельный лимит и отсутствие учёта в доставке.
func TestControlRequests(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{}, 4)
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}

		<-release

		_, errWrite := io.WriteString(w, `{"ok":true,"result":true}`)
		if errWrite != nil {
			t.Error(errWrite)
		}
	}))
	defer server.Close()

	client, errNew := New(testToken, func(context.Context, Message) {}, WithServerURL(server.URL))
	if errNew != nil {
		t.Fatal(errNew)
	}

	results := make(chan error, 4)

	for range 4 {
		go func() { results <- client.AnswerCallback(context.Background(), "query", "", false) }()
	}

	for range 4 {
		<-entered
	}

	errBusy := client.AnswerCallback(context.Background(), "overflow", "", false)
	if !errors.Is(errBusy, ErrBusy) {
		t.Errorf("overflow = %v", errBusy)
	}

	close(release)

	for range 4 {
		errAnswer := <-results
		if errAnswer != nil {
			t.Error(errAnswer)
		}
	}

	if client.Stats().Sent != 0 || client.Stats().Dropped != 0 {
		t.Fatal("control requests changed delivery counters")
	}
}

// TestDeliveryLimitsStartAfterResponse отсчитывает интервалы смешанной очереди после завершения HTTP.
func TestDeliveryLimitsStartAfterResponse(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name            string
		nextChat        int64
		perChat, global time.Duration
	}{
		{name: "same chat", nextChat: 1, perChat: 200 * time.Millisecond, global: time.Millisecond},
		{name: "global", nextChat: 2, perChat: time.Millisecond, global: 200 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var clockMu sync.Mutex

			now := time.Unix(1000, 0)
			clock := func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()

				return now
			}
			firstStarted := make(chan struct{})
			releaseFirst := make(chan struct{})
			secondStarted := make(chan time.Time, 1)
			attempts := 0

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clockMu.Lock()
				attempts++
				attempt := attempts
				clockMu.Unlock()

				if attempt == 1 {
					close(firstStarted)

					select {
					case <-releaseFirst:
					case <-r.Context().Done():
						return
					}
				} else {
					secondStarted <- clock()
				}

				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":1,"type":"private"}}}`)
			}))
			defer server.Close()

			waits := make(chan time.Duration, 1)

			client, errNew := New(testToken, func(context.Context, Message) {}, WithServerURL(server.URL), withSleep(func(ctx context.Context, pause time.Duration) bool {
				waits <- pause

				clockMu.Lock()
				now = now.Add(pause)
				clockMu.Unlock()

				return ctx.Err() == nil
			}))
			if errNew != nil {
				t.Fatal(errNew)
			}

			client.limiter = newLimiter(test.perChat, test.global, clock)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			done := make(chan bool, 1)

			go func() {
				first := client.deliver(ctx, outgoing{chatID: 1, delivery: Delivery{Kind: DeliveryPhoto, PNG: []byte("png")}})

				second := client.deliver(ctx, outgoing{chatID: test.nextChat, text: "text", delivery: Delivery{Kind: DeliveryText, Text: "text"}})
				done <- first && second
			}()

			select {
			case <-firstStarted:
			case <-ctx.Done():
				t.Fatal("first request did not start")
			}

			clockMu.Lock()
			now = now.Add(100 * time.Millisecond)
			completed := now
			clockMu.Unlock()
			close(releaseFirst)

			select {
			case pause := <-waits:
				if pause != 200*time.Millisecond {
					t.Errorf("pause=%s, want full interval after response", pause)
				}
			case <-ctx.Done():
				t.Fatal("next request did not wait")
			}

			awaitRateLimitedCompletion(t, ctx, completed, secondStarted, done)
		})
	}
}

// awaitRateLimitedCompletion проверяет начало второй попытки и завершение доставок.
func awaitRateLimitedCompletion(t *testing.T, ctx context.Context, completed time.Time, secondStarted <-chan time.Time, done <-chan bool) {
	t.Helper()

	select {
	case started := <-secondStarted:
		if started.Before(completed.Add(200 * time.Millisecond)) {
			t.Errorf("next request started %s after completion", started.Sub(completed))
		}
	case <-ctx.Done():
		t.Fatal("second request did not start")
	}

	select {
	case delivered := <-done:
		if !delivered {
			t.Fatal("delivery canceled")
		}
	case <-ctx.Done():
		t.Fatal("delivery did not finish")
	}
}
