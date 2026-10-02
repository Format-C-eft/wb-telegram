package telegram

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// slowListener задерживает каждое принятое соединение, имитируя медленное TLS-рукопожатие.
type slowListener struct {
	net.Listener

	delay time.Duration
}

// Accept принимает соединение и выдерживает паузу до начала рукопожатия.
func (l slowListener) Accept() (net.Conn, error) {
	conn, errAccept := l.Listener.Accept()
	if errAccept != nil {
		return nil, errAccept
	}

	time.Sleep(l.delay)

	return conn, nil
}

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
}

// TestNewHTTPClientSlowHandshake проверяет, что рукопожатие дольше таймаута обрывается, а в пределах — проходит.
func TestNewHTTPClientSlowHandshake(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		connect time.Duration
		wantErr bool
	}{
		{name: "handshake slower than timeout", connect: 100 * time.Millisecond, wantErr: true},
		{name: "handshake within timeout", connect: 2 * time.Second, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			server.Listener = slowListener{Listener: server.Listener, delay: 400 * time.Millisecond}
			server.StartTLS()
			t.Cleanup(server.Close)

			client := newHTTPClient(tt.connect, time.Second)

			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			client.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots

			request, errRequest := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
			if errRequest != nil {
				t.Fatalf("NewRequest: %v", errRequest)
			}

			response, errDo := client.Do(request)
			if errDo == nil {
				_ = response.Body.Close()
			}

			if tt.wantErr && errDo == nil {
				t.Fatal("request succeeded, want handshake timeout")
			}

			if !tt.wantErr && errDo != nil {
				t.Fatalf("request failed: %v", errDo)
			}
		})
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
