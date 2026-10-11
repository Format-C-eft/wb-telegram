package telegram

import (
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// blackholeProxy пересылает TCP к серверу; после freeze уже открытые соединения
// перестают пересылать данные (как после перезагрузки роутера), новые работают.
type blackholeProxy struct {
	listener net.Listener
	target   string

	mu     sync.Mutex
	frozen map[net.Conn]bool
	conns  []net.Conn
}

// newBlackholeProxy запускает прокси к target на 127.0.0.1.
func newBlackholeProxy(t *testing.T, target string) *blackholeProxy {
	t.Helper()

	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}

	p := &blackholeProxy{listener: listener, target: target, frozen: map[net.Conn]bool{}}

	t.Cleanup(p.close)

	go p.serve()

	return p
}

// serve принимает соединения и пересылает их к серверу.
func (p *blackholeProxy) serve() {
	for {
		client, errAccept := p.listener.Accept()
		if errAccept != nil {
			return
		}

		server, errDial := net.Dial("tcp", p.target)
		if errDial != nil {
			_ = client.Close()

			continue
		}

		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()

		go p.pipe(client, server, client)
		go p.pipe(server, client, client)
	}
}

// pipe копирует данные src → dst, пока соединение key не заморожено.
func (p *blackholeProxy) pipe(src, dst, key net.Conn) {
	buffer := make([]byte, 32*1024)

	for {
		n, errRead := src.Read(buffer)
		if errRead != nil {
			return
		}

		if p.isFrozen(key) {
			_, _ = io.Copy(io.Discard, src)

			return
		}

		_, errWrite := dst.Write(buffer[:n])
		if errWrite != nil {
			return
		}
	}
}

// freeze замораживает все уже открытые соединения.
func (p *blackholeProxy) freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, conn := range p.conns {
		p.frozen[conn] = true
	}
}

// isFrozen сообщает, заморожено ли соединение.
func (p *blackholeProxy) isFrozen(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.frozen[conn]
}

// close останавливает прокси и закрывает соединения.
func (p *blackholeProxy) close() {
	_ = p.listener.Close()

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, conn := range p.conns {
		_ = conn.Close()
	}
}

// TestHTTPClientRecoversFromDeadConnection проверяет, что после «тихой» смерти HTTP/2-соединения
// (пакеты пропадают, сокет не закрыт) клиент сам переходит на новое соединение.
func TestHTTPClientRecoversFromDeadConnection(t *testing.T) {
	t.Parallel()

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	proxy := newBlackholeProxy(t, server.Listener.Addr().String())

	client := newHTTPClient(500*time.Millisecond, time.Second)

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots

	url := "https://" + proxy.listener.Addr().String() + "/"

	// get выполняет запрос и сообщает версию протокола (0 — ошибка).
	get := func() int {
		request, errRequest := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if errRequest != nil {
			t.Fatalf("NewRequest: %v", errRequest)
		}

		response, errDo := client.Do(request)
		if errDo != nil {
			return 0
		}

		_ = response.Body.Close()

		return response.ProtoMajor
	}

	if proto := get(); proto != 2 {
		t.Fatalf("first request protocol = %d, want HTTP/2", proto)
	}

	proxy.freeze()

	deadline := time.Now().Add(15 * time.Second)

	for get() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("client did not switch to a new connection after the old one died")
		}
	}
}
