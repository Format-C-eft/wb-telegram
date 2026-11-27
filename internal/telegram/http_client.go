package telegram

import (
	"net/http"
	"net/http/httptrace"
)

//go:generate ../../bin/mockgen -source=http_client.go -destination=mock/http_client_mock.go -package=mock

// HTTPClient выполняет HTTP-запросы Bot API.
type HTTPClient interface {
	// Do выполняет HTTP-запрос.
	Do(*http.Request) (*http.Response, error)
}

// healthClient оборачивает HTTP-клиент Bot API: ответ сервера или новое соединение значит, что связь жива.
type healthClient struct {
	inner HTTPClient
	alive func()
}

// Do выполняет запрос и отмечает связь живой по новому соединению и по любому ответу ниже 500.
func (h *healthClient) Do(req *http.Request) (*http.Response, error) {
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if !info.Reused {
				h.alive()
			}
		},
	}

	resp, errDo := h.inner.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if errDo != nil {
		return nil, errDo
	}

	if resp.StatusCode < http.StatusInternalServerError {
		h.alive()
	}

	return resp, nil
}
