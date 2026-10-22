package telegram

import (
	"net/http"
	"net/http/httptrace"
)

// healthClient оборачивает HTTP-клиент Bot API: ответ сервера или новое соединение значит, что связь жива.
type healthClient struct {
	inner *http.Client
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

	resp, errDo := h.inner.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace))) //nolint:gosec // адрес Bot API задан настройками, не вводом пользователя
	if errDo != nil {
		return nil, errDo
	}

	if resp.StatusCode < http.StatusInternalServerError {
		h.alive()
	}

	return resp, nil
}
