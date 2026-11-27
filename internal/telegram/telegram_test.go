package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/telegram/mock"

	"github.com/Format-C-eft/wb-telegram/internal/retry"
)

// startClient создаёт и запускает клиента против поддельного API.
func startClient(t *testing.T, api *fakeAPI, handler Handler, opts ...Option) *Client {
	t.Helper()

	base := []Option{WithServerURL("https://api.telegram.test"), WithHTTPClient(api.client), WithRateLimits(time.Millisecond, time.Millisecond), WithRetry(time.Millisecond, 10*time.Millisecond)}

	client, errNew := New(testToken, handler, append(base, opts...)...)
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	errRun := client.Run(context.Background())
	if errRun != nil {
		t.Fatalf("Run: %v", errRun)
	}

	t.Cleanup(func() { _ = client.Stop() })

	return client
}

// TestNewRejectsEmptyToken проверяет отказ на пустом токене.
func TestNewRejectsEmptyToken(t *testing.T) {
	t.Parallel()

	_, errNew := New(" ", func(context.Context, Message) {})
	if !errors.Is(errNew, ErrInvalidArgument) {
		t.Fatalf("New error = %v, want ErrInvalidArgument", errNew)
	}
}

// TestDispatchDeliversTextMessages проверяет доставку текстовых сообщений обработчику.
func TestDispatchDeliversTextMessages(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)

	var (
		mu  sync.Mutex
		got []Message
	)

	startClient(t, api, func(_ context.Context, msg Message) {
		mu.Lock()

		got = append(got, msg)

		mu.Unlock()
	})

	date := time.Unix(1759312345, 0)
	api.queueUpdate(42, 100, "/gate 22", date)

	waitFor(t, "message delivered", func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(got) == 1
	})

	mu.Lock()
	defer mu.Unlock()

	want := Message{UpdateID: 42, ChatID: 100, Text: "/gate 22", Date: date}
	if got[0] != want {
		t.Fatalf("message = %+v, want %+v", got[0], want)
	}
}

// TestDispatchOrdersCommandsAndChecksRecipient проверяет порядок и адресата групповых команд.
func TestDispatchOrdersCommandsAndChecksRecipient(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	delivered := make(chan string, 3)

	unblock := make(chan struct{}, 1)
	defer close(unblock)

	started := make(chan struct{}, 1)

	api.queueRawUpdate(`{"update_id":40,"message":{"message_id":4,"date":1,"chat":{"id":-100,"type":"group"},"text":"/gate@HomeAlias"}}`)
	api.queueRawUpdate(`{"update_id":41,"message":{"message_id":1,"date":1,"chat":{"id":-100,"type":"group"},"text":"/gate@OtherBot"}}`)
	api.queueRawUpdate(`{"update_id":42,"message":{"message_id":2,"date":1,"chat":{"id":-100,"type":"group"},"text":"/gate@myhomebot"}}`)
	api.queueRawUpdate(`{"update_id":43,"message":{"message_id":3,"date":1,"chat":{"id":-100,"type":"group"},"text":"/gate 2"}}`)

	startClient(t, api, func(ctx context.Context, msg Message) {
		if msg.UpdateID == 42 {
			started <- struct{}{}

			select {
			case <-unblock:
			case <-ctx.Done():
			}
		}

		delivered <- msg.Text
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first addressed command did not start")
	}

	select {
	case text := <-delivered:
		t.Fatalf("unexpected command before first handler returned: %s", text)
	case <-time.After(100 * time.Millisecond):
	}

	unblock <- struct{}{}

	for _, want := range []string{"/gate@myhomebot", "/gate 2"} {
		select {
		case got := <-delivered:
			if got != want {
				t.Fatalf("command = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("command %q was not delivered", want)
		}
	}

	api.mu.Lock()
	defer api.mu.Unlock()

	if api.getMeCalls != 1 {
		t.Fatalf("getMe calls = %d, want cached bot username", api.getMeCalls)
	}
}

// TestDispatchIgnoresNonText проверяет, что сообщения без текста не доходят до обработчика.
func TestDispatchIgnoresNonText(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)

	var (
		mu  sync.Mutex
		got []Message
	)

	startClient(t, api, func(_ context.Context, msg Message) {
		mu.Lock()

		got = append(got, msg)

		mu.Unlock()
	})

	api.queueRawUpdate(`{"update_id":1,"message":{"message_id":1,"date":1,"chat":{"id":100,"type":"private"},"sticker":{"file_id":"x","file_unique_id":"y","type":"regular","width":1,"height":1,"is_animated":false,"is_video":false}}}`)
	api.queueUpdate(2, 100, "after", time.Now())

	waitFor(t, "text message delivered", func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(got) == 1
	})

	mu.Lock()
	defer mu.Unlock()

	if got[0].Text != "after" {
		t.Fatalf("got %+v", got)
	}
}

// TestRunDeletesWebhook проверяет вызов deleteWebhook при старте.
func TestRunDeletesWebhook(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	startClient(t, api, func(context.Context, Message) {})

	waitFor(t, "webhook deleted", func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()

		return api.webhookDeleted
	})
}

// TestUnauthorizedIsFatal проверяет вызов onFatal с ErrUnauthorized при отклонённом токене.
func TestUnauthorizedIsFatal(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	api.setUnauthorized()

	fatal := make(chan error, 4)

	startClient(t, api, func(context.Context, Message) {}, WithOnFatal(func(err error) {
		select {
		case fatal <- err:
		default:
		}
	}))

	select {
	case err := <-fatal:
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("fatal error = %v, want ErrUnauthorized", err)
		}

		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("token leaked into error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onFatal was not called")
	}
}

// TestSetCommands проверяет установку меню команд для конкретного чата.
func TestSetCommands(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	client := startClient(t, api, func(context.Context, Message) {})

	errSet := client.SetCommands(context.Background(), 100, []Command{{Name: "gate", Description: "Ворота"}})
	if errSet != nil {
		t.Fatalf("SetCommands: %v", errSet)
	}

	errEmpty := client.SetCommands(context.Background(), 200, nil)
	if errEmpty != nil {
		t.Fatalf("SetCommands empty: %v", errEmpty)
	}

	api.mu.Lock()
	defer api.mu.Unlock()

	if got := api.commands[`{"type":"chat","chat_id":100}`]; got != `[{"command":"gate","description":"Ворота"}]` {
		t.Fatalf("commands for 100 = %q (all: %v)", got, api.commands)
	}

	if got := api.commands[`{"type":"chat","chat_id":200}`]; got != `[]` {
		t.Fatalf("commands for 200 = %q", got)
	}
}

// TestSetCommandsErrorHidesToken проверяет, что текст ошибки SetCommands не содержит токен.
func TestSetCommandsErrorHidesToken(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	client := startClient(t, api, func(context.Context, Message) {})

	api.setUnauthorized()

	errSet := client.SetCommands(context.Background(), 100, nil)
	if !errors.Is(errSet, ErrUnauthorized) {
		t.Fatalf("SetCommands error = %v, want ErrUnauthorized", errSet)
	}

	if strings.Contains(errSet.Error(), "secret") {
		t.Fatalf("token leaked into error: %v", errSet)
	}
}

// TestMaskToken проверяет маскирование токена в тексте ошибок.
func TestMaskToken(t *testing.T) {
	t.Parallel()

	client, errNew := New(testToken, func(context.Context, Message) {})
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	got := client.mask("Post https://api.telegram.org/bot123:secret/getUpdates: timeout")
	if strings.Contains(got, "secret") || !strings.Contains(got, "bot***") {
		t.Fatalf("mask = %q", got)
	}
}

// TestTransportErrorHidesToken проверяет, что URL с токеном в сетевой ошибке маскируется.
func TestTransportErrorHidesToken(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	client := startClient(t, api, func(context.Context, Message) {})

	api.close()

	errSet := client.SetCommands(context.Background(), 100, nil)
	if !errors.Is(errSet, ErrTransient) {
		t.Fatalf("SetCommands error = %v, want ErrTransient", errSet)
	}

	if strings.Contains(errSet.Error(), "secret") || !strings.Contains(errSet.Error(), "bot***") {
		t.Fatalf("error text = %q, want masked token", errSet.Error())
	}
}

// TestCanceledContextIsNotFailure проверяет, что отмена контекста не считается сбоем связи.
func TestCanceledContextIsNotFailure(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	client := startClient(t, api, func(context.Context, Message) {})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	errSet := client.SetCommands(ctx, 100, nil)
	if errSet == nil {
		t.Fatal("SetCommands with canceled context returned nil")
	}

	if client.failing.Load() {
		t.Fatal("canceled context marked the connection as failing")
	}
}

// TestOnPollErrorClassification проверяет разбор ошибок polling.
func TestOnPollErrorClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		err         error
		wantFatal   bool
		wantFailing bool
	}{
		{name: "unauthorized", err: fmt.Errorf("error get updates, %w", bot.ErrorUnauthorized), wantFatal: true},
		{name: "decode error without raw text", err: errors.New("error decode update, {\"text\":\"private\"}, bad json")},
		{name: "updates lost on shutdown", err: errors.New("some updates lost, ctx done")},
		{name: "canceled", err: fmt.Errorf("error get updates, %w", context.Canceled)},
		{name: "network", err: errors.New("error get updates, connection refused"), wantFailing: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var fatal atomic.Bool

			client, errNew := New(testToken, func(context.Context, Message) {}, WithOnFatal(func(error) { fatal.Store(true) }))
			if errNew != nil {
				t.Fatalf("New: %v", errNew)
			}

			client.onPollError(tt.err)

			if fatal.Load() != tt.wantFatal {
				t.Fatalf("fatal = %v, want %v", fatal.Load(), tt.wantFatal)
			}

			if client.failing.Load() != tt.wantFailing {
				t.Fatalf("failing = %v, want %v", client.failing.Load(), tt.wantFailing)
			}
		})
	}
}

// TestRunTwiceFails проверяет отказ на повторном Run и после Stop.
func TestRunTwiceFails(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	client := startClient(t, api, func(context.Context, Message) {})

	if errRun := client.Run(context.Background()); !errors.Is(errRun, ErrInvalidArgument) {
		t.Fatalf("second Run error = %v, want ErrInvalidArgument", errRun)
	}

	_ = client.Stop()

	if errRun := client.Run(context.Background()); !errors.Is(errRun, ErrClosed) {
		t.Fatalf("Run after Stop error = %v, want ErrClosed", errRun)
	}
}

// TestDeleteWebhookRejectedStopsRetrying проверяет, что отказ 400 не приводит к бесконечным повторам.
func TestDeleteWebhookRejectedStopsRetrying(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	api.setWebhookStatus("400")

	client := startClient(t, api, func(context.Context, Message) {})

	waitFor(t, "deleteWebhook call", func() bool { return api.webhookCallCount() >= 1 })

	time.Sleep(100 * time.Millisecond)

	_ = client.Stop()

	if got := api.webhookCallCount(); got != 1 {
		t.Fatalf("deleteWebhook calls = %d, want 1", got)
	}
}

// TestDeleteWebhookHonorsRetryAfter проверяет, что при 429 пауза не меньше retry_after.
func TestDeleteWebhookHonorsRetryAfter(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	api.setWebhookStatus("429")

	pauses := make(chan time.Duration, 4)

	startClient(t, api, func(context.Context, Message) {}, withSleep(func(ctx context.Context, d time.Duration) bool {
		select {
		case pauses <- d:
		default:
		}

		return retry.Sleep(ctx, time.Millisecond)
	}))

	select {
	case got := <-pauses:
		if got < time.Second {
			t.Fatalf("pause = %v, want >= 1s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no retry pause observed")
	}
}

// TestCallbackPollingAndAnswer проверяет обычный callback и подтверждение через Bot API.
func TestCallbackPollingAndAnswer(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	received := make(chan Callback, 1)
	client := startClient(t, api, func(context.Context, Message) {
		t.Error("callback was delivered as text")
	}, WithCallbackHandler(func(_ context.Context, callback Callback) { received <- callback }))

	api.queueRawUpdate(`{"update_id":17,"callback_query":{"id":"press-1","from":{"id":9,"is_bot":false,"first_name":"User"},"message":{"message_id":3,"date":100,"chat":{"id":7,"type":"private"}},"data":"open-gate"}}`)

	select {
	case got := <-received:
		want := Callback{UpdateID: 17, QueryID: "press-1", FromID: 9, ChatID: 7, ChatType: "private", MessageID: 3, Data: "open-gate", Accessible: true}
		if got != want {
			t.Fatalf("callback = %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("callback was not delivered")
	}

	errAnswer := client.AnswerCallback(t.Context(), "press-1", "Opened", true)
	if errAnswer != nil {
		t.Fatal(errAnswer)
	}

	api.mu.Lock()
	defer api.mu.Unlock()

	if answer := api.callbackAnswers["press-1"]; answer != "Opened:true" {
		t.Fatalf("callback answer = %q", answer)
	}
}

// TestShutdownCancelsPendingHTTP проверяет завершение HTTP polling и отправки до возврата Shutdown.
func TestShutdownCancelsPendingHTTP(t *testing.T) {
	t.Parallel()

	inner := mock.NewMockHTTPClient(gomock.NewController(t))
	pollEntered := make(chan struct{})
	pollReturned := make(chan struct{})
	sendEntered := make(chan struct{})
	sendReturned := make(chan struct{})

	inner.EXPECT().Do(gomock.Any()).DoAndReturn(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Host != "api.telegram.test" {
			t.Errorf("unexpected HTTP request: %s %s", request.Method, request.URL)
		}

		switch request.URL.Path {
		case "/bot" + testToken + "/deleteWebhook":
			recorder := httptest.NewRecorder()
			writeResult(recorder, "true")

			return recorder.Result(), nil
		case "/bot" + testToken + "/getUpdates":
			close(pollEntered)
			<-request.Context().Done()
			close(pollReturned)
		case "/bot" + testToken + "/sendMessage":
			close(sendEntered)
			<-request.Context().Done()
			close(sendReturned)
		default:
			t.Errorf("unexpected method: %s", request.URL.Path)

			return nil, errors.New("unexpected method")
		}

		return nil, request.Context().Err()
	}).Times(3)

	client, errNew := New(testToken, func(context.Context, Message) {}, WithHTTPClient(inner), WithServerURL("https://api.telegram.test"))
	if errNew != nil {
		t.Fatal(errNew)
	}

	errRun := client.Run(t.Context())
	if errRun != nil {
		t.Fatal(errRun)
	}

	t.Cleanup(func() { _ = client.Stop() })

	errSend := client.Send(7, "pending")
	if errSend != nil {
		t.Fatal(errSend)
	}

	for _, entered := range []<-chan struct{}{pollEntered, sendEntered} {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("HTTP request did not start")
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	errShutdown := client.Shutdown(ctx)
	if errShutdown != nil {
		t.Fatal(errShutdown)
	}

	for _, completed := range []<-chan struct{}{pollReturned, sendReturned, client.pollDone, client.sendDone, client.webhookDone} {
		select {
		case <-completed:
		default:
			t.Fatal("Shutdown returned before HTTP goroutines finished")
		}
	}
}
