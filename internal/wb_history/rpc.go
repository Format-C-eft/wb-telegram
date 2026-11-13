package wb_history

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	paho "github.com/eclipse/paho.mqtt.golang"
)

const maxResponseBytes = 512 * 1024
const maxPoints = 1000

type request struct {
	ID     string     `json:"id"`
	Params parameters `json:"params"`
}
type parameters struct {
	Version      int         `json:"ver"`
	Channels     [][2]string `json:"channels"`
	Timestamp    interval    `json:"timestamp"`
	MaxRecords   int         `json:"max_records"`
	Limit        int         `json:"limit"`
	Milliseconds bool        `json:"with_milliseconds"`
	Timeout      int         `json:"request_timeout"`
}
type interval struct {
	GT float64 `json:"gt"`
	LT float64 `json:"lt"`
}

// ValidIdentifier проверяет имя устройства или контрола MQTT.
func ValidIdentifier(value string) bool {
	if !utf8.ValidString(value) || value == "" || len(value) > 128 || strings.ContainsAny(value, "/+#") {
		return false
	}

	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}

	return true
}

// Read запрашивает историю одного канала без повторов и пагинации.
func (c *Client) Read(ctx context.Context, query Query) ([]Point, error) {
	if ctx.Err() != nil {
		return nil, contextError(ctx)
	}

	if !ValidIdentifier(query.Device) || !ValidIdentifier(query.Control) || !query.Start.Before(query.End) {
		return nil, ErrInvalidArgument
	}

	deadline := time.Now().Add(8 * time.Second)
	if outer, ok := ctx.Deadline(); ok && outer.Before(deadline) {
		deadline = outer
	}

	seconds := int(time.Until(deadline) / time.Second)
	if seconds < 1 {
		return nil, ErrTimeout
	}

	requestContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	current, errReserve := c.reserve(query)
	if errReserve != nil {
		return nil, errReserve
	}

	defer c.clear(current)

	payload, errMarshal := json.Marshal(request{ID: current.id, Params: parameters{Version: 1, Channels: [][2]string{{query.Device, query.Control}}, Timestamp: interval{GT: secondsUnix(query.Start), LT: secondsUnix(query.End)}, MaxRecords: 800, Limit: 1001, Milliseconds: true, Timeout: min(seconds, 8)}})
	if errMarshal != nil {
		return nil, ErrInvalidArgument
	}

	c.mu.Lock()
	if requestContext.Err() != nil {
		c.mu.Unlock()

		return nil, contextError(requestContext)
	}

	if c.closed || !c.ready || c.pending != current || !c.client.IsConnectionOpen() {
		c.mu.Unlock()

		return nil, ErrUnavailable
	}

	token := c.client.Publish(c.topic, 1, false, payload)
	c.mu.Unlock()

	return c.awaitResponse(requestContext, current, token)
}

// awaitResponse ожидает публикацию или ответ без повторной отправки.
func (c *Client) awaitResponse(ctx context.Context, current *pending, token paho.Token) ([]Point, error) {
	select {
	case <-ctx.Done():
		return nil, contextError(ctx)
	case <-c.stop:
		return nil, ErrClosed
	case result := <-current.result:
		if result.err != nil {
			return nil, result.err
		}

		return result.points, nil
	case <-token.Done():
		if token.Error() != nil {
			return nil, ErrUnavailable
		}
	}

	select {
	case <-ctx.Done():
		return nil, contextError(ctx)
	case <-c.stop:
		return nil, ErrClosed
	case result := <-current.result:
		if result.err != nil {
			return nil, result.err
		}

		return result.points, nil
	}
}

// clear освобождает место активного запроса.
func (c *Client) clear(current *pending) {
	c.mu.Lock()
	if c.pending == current {
		c.pending = nil
	}
	c.mu.Unlock()
}

// secondsUnix возвращает Unix-время с долями секунды.
func secondsUnix(value time.Time) float64 {
	return float64(value.Unix()) + float64(value.Nanosecond())/1e9
}

// reserve регистрирует единственное ожидание ответа.
func (c *Client) reserve(query Query) (*pending, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()

		return nil, ErrClosed
	}

	if !c.ready || !c.client.IsConnectionOpen() {
		c.mu.Unlock()

		return nil, ErrUnavailable
	}

	if c.pending != nil {
		c.mu.Unlock()

		return nil, ErrBusy
	}

	c.sequence++
	current := &pending{id: strconv.FormatUint(c.sequence, 10), query: query, result: make(chan response, 1)}
	c.pending = current
	c.mu.Unlock()

	return current, nil
}

// contextError различает отмену и исчерпание локального срока.
func contextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return ErrCanceled
	}

	return ErrTimeout
}
