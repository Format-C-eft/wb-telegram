package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/telegram/mock"
)

// TestHealthClientRecoversFromFailedRequest проверяет восстановление после ошибки HTTP-запроса.
func TestHealthClientRecoversFromFailedRequest(t *testing.T) {
	t.Parallel()

	inner := mock.NewMockHTTPClient(gomock.NewController(t))
	response := &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}
	gomock.InOrder(
		inner.EXPECT().Do(gomock.Any()).Return(nil, io.ErrUnexpectedEOF),
		inner.EXPECT().Do(gomock.Any()).Return(response, nil),
	)

	alive := 0
	client := &healthClient{inner: inner, alive: func() { alive++ }}

	request, errRequest := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.telegram.test", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}

	failedResponse, errFailed := client.Do(request)
	if !errors.Is(errFailed, io.ErrUnexpectedEOF) {
		t.Fatalf("request error = %v, want unexpected EOF", errFailed)
	}

	if failedResponse != nil || alive != 0 {
		t.Fatalf("failed response = %v; healthy notifications = %d", failedResponse, alive)
	}

	got, errDo := client.Do(request)
	if errDo != nil {
		t.Fatal(errDo)
	}

	if got != response || alive != 1 {
		t.Fatalf("recovery response = %v; healthy notifications = %d", got, alive)
	}
}

// TestHealthClientConnectionTrace проверяет здоровье нового и повторно используемого соединения.
func TestHealthClientConnectionTrace(t *testing.T) {
	t.Parallel()

	for _, scenario := range []struct {
		name      string
		reused    bool
		status    int
		wantAlive int
	}{
		{name: "new connection despite server error", status: http.StatusInternalServerError, wantAlive: 1},
		{name: "reused connection server error", reused: true, status: http.StatusInternalServerError},
		{name: "reused connection rate limit", reused: true, status: http.StatusTooManyRequests, wantAlive: 1},
		{name: "new connection successful response", status: http.StatusOK, wantAlive: 2},
	} {
		t.Run(scenario.name, func(subtest *testing.T) {
			subtest.Parallel()

			inner := mock.NewMockHTTPClient(gomock.NewController(subtest))
			inner.EXPECT().Do(gomock.Any()).DoAndReturn(func(request *http.Request) (*http.Response, error) {
				trace := httptrace.ContextClientTrace(request.Context())
				if trace == nil || trace.GotConn == nil {
					subtest.Fatal("HTTP connection trace was not installed")
				}

				trace.GotConn(httptrace.GotConnInfo{Reused: scenario.reused})

				return &http.Response{StatusCode: scenario.status, Body: http.NoBody}, nil
			})

			alive := 0
			client := &healthClient{inner: inner, alive: func() { alive++ }}

			request, errRequest := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.telegram.test", nil)
			if errRequest != nil {
				subtest.Fatal(errRequest)
			}

			_, errDo := client.Do(request)
			if errDo != nil {
				subtest.Fatal(errDo)
			}

			if alive != scenario.wantAlive {
				subtest.Fatalf("healthy notifications = %d, want %d", alive, scenario.wantAlive)
			}
		})
	}
}
