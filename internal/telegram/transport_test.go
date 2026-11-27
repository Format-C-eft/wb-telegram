package telegram

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/telegram/mock"
)

// TestNewHTTPClientTimeouts проверяет, что таймауты подключения и запроса берутся из настроек.
func TestNewHTTPClientTimeouts(t *testing.T) {
	t.Parallel()

	client := newHTTPClient(45*time.Second, 25*time.Second)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}

	if transport.TLSHandshakeTimeout != 45*time.Second {
		t.Fatalf("TLSHandshakeTimeout = %v, want 45s", transport.TLSHandshakeTimeout)
	}

	if client.Timeout != 45*time.Second+25*time.Second+requestMargin {
		t.Fatalf("Timeout = %v, want connect + poll + margin", client.Timeout)
	}

	if transport.HTTP2 == nil || transport.HTTP2.SendPingTimeout != maxPingInterval || transport.HTTP2.PingTimeout != 45*time.Second || transport.HTTP2.WriteByteTimeout != 45*time.Second {
		t.Fatalf("HTTP2 = %+v, want ping after %v and ping timeout 45s", transport.HTTP2, maxPingInterval)
	}
}

// TestHealthClientCancellation проверяет отмену выполняющегося запроса HTTP-клиента.
func TestHealthClientCancellation(t *testing.T) {
	t.Parallel()

	inner := mock.NewMockHTTPClient(gomock.NewController(t))
	entered := make(chan struct{})

	inner.EXPECT().Do(gomock.Any()).DoAndReturn(func(request *http.Request) (*http.Response, error) {
		close(entered)
		<-request.Context().Done()

		return nil, request.Context().Err()
	})

	alive := false
	client := &healthClient{inner: inner, alive: func() { alive = true }}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.telegram.test", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}

	result := make(chan error, 1)

	go func() {
		_, errDo := client.Do(request)
		result <- errDo
	}()

	<-entered
	cancel()

	select {
	case errResult := <-result:
		if !errors.Is(errResult, context.Canceled) {
			t.Fatalf("request error = %v, want cancellation", errResult)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request did not finish")
	}

	if alive {
		t.Fatal("cancellation marked the connection healthy")
	}
}

// TestWithTimeoutsValidation проверяет отказ на нулевых таймаутах.
func TestWithTimeoutsValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		connect time.Duration
		poll    time.Duration
	}{
		{name: "zero connect", connect: 0, poll: time.Second},
		{name: "zero poll", connect: time.Second, poll: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, errNew := New(testToken, func(context.Context, Message) {}, WithTimeouts(tt.connect, tt.poll))
			if !errors.Is(errNew, ErrInvalidArgument) {
				t.Fatalf("New error = %v, want ErrInvalidArgument", errNew)
			}
		})
	}
}
