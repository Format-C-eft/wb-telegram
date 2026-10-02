package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"

	"github.com/Format-C-eft/wb-telegram/internal/retry"
)

// startClient создаёт и запускает клиента против поддельного API.
func startClient(t *testing.T, api *fakeAPI, handler Handler, opts ...Option) *Client {
	t.Helper()

	base := []Option{WithServerURL(api.server.URL), WithRateLimits(time.Millisecond, time.Millisecond), WithRetry(time.Millisecond, 10*time.Millisecond)}

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

	api.server.Close()

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
