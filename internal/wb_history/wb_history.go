// Package wb_history читает числовую историю штатным MQTT RPC wb-mqtt-db.
package wb_history

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

type kind uint8

const (
	kindInvalid kind = iota + 1
	kindUnavailable
	kindBusy
	kindClosed
	kindTimeout
	kindResponse
	kindContract
	kindTooMany
	kindServiceTimeout
	kindCanceled
)

// Error представляет отказ чтения истории.
type Error struct{ kind kind }

// Error возвращает безопасное описание отказа.
func (e *Error) Error() string {
	switch e.kind {
	case kindInvalid:
		return "некорректный запрос истории"
	case kindUnavailable:
		return "история временно недоступна"
	case kindBusy:
		return "чтение истории уже выполняется"
	case kindClosed:
		return "клиент истории остановлен"
	case kindCanceled:
		return "чтение истории отменено"
	case kindServiceTimeout:
		return "база не успела подготовить историю"
	case kindTimeout:
		return "время чтения истории истекло"
	case kindContract:
		return "неподдерживаемый формат истории"
	case kindTooMany:
		return "слишком много данных: уменьшите период"
	default:
		return "некорректный ответ истории"
	}
}

// Is сравнивает вид ошибки.
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)

	return ok && e.kind == other.kind
}

var (
	ErrInvalidArgument = &Error{kind: kindInvalid}
	ErrUnavailable     = &Error{kind: kindUnavailable}
	ErrBusy            = &Error{kind: kindBusy}
	ErrClosed          = &Error{kind: kindClosed}
	ErrTimeout         = &Error{kind: kindTimeout}
	ErrServiceTimeout  = &Error{kind: kindServiceTimeout}
	ErrCanceled        = &Error{kind: kindCanceled}
	ErrInvalidResponse = &Error{kind: kindResponse}
	ErrUnsupported     = &Error{kind: kindContract}
	ErrTooManyData     = &Error{kind: kindTooMany}
)

// Query задаёт открытый интервал истории одного канала.
type Query struct {
	Device, Control string
	Start, End      time.Time
}

// Point хранит измерение; Valid=false обозначает разрыв ряда.
type Point struct {
	Time                  time.Time
	Value, Min, Max       float64
	Valid, HasMin, HasMax bool
}

type pending struct {
	id     string
	query  Query
	result chan response
}
type response struct {
	points []Point
	err    error
}
type incoming struct {
	payload   []byte
	oversized bool
	pending   *pending
}

// Client читает историю через отдельное MQTT-соединение.
type Client struct {
	options                options
	client                 paho.Client
	topic                  string
	mu                     sync.Mutex
	setups                 sync.WaitGroup
	started, closed, ready bool
	generation, sequence   uint64
	pending                *pending
	messages               chan incoming
	stop                   chan struct{}
	done                   chan struct{}
}

// New создаёт клиент без подключения к брокеру.
func New(opts ...Option) (*Client, error) {
	o := options{broker: "tcp://127.0.0.1:1883"}

	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	errValidate := o.Validate()
	if errValidate != nil {
		return nil, errValidate
	}

	var nonce [12]byte

	_, errRandom := rand.Read(nonce[:])
	if errRandom != nil {
		return nil, ErrUnavailable
	}

	id := "wb-telegram-history-" + hex.EncodeToString(nonce[:])
	c := &Client{options: o, topic: "/rpc/v1/db_logger/history/get_values/" + id, messages: make(chan incoming, 1), stop: make(chan struct{}), done: make(chan struct{})}
	config := paho.NewClientOptions().AddBroker(o.broker).SetClientID(id).SetCleanSession(true).SetAutoReconnect(true).SetConnectRetry(true).SetConnectTimeout(time.Second).SetConnectRetryInterval(time.Second).SetMaxReconnectInterval(5 * time.Second).SetWriteTimeout(time.Second).SetResumeSubs(false).SetStore(discardStore{}).SetOnConnectHandler(c.onConnect).SetConnectionLostHandler(c.onLost)
	c.client = paho.NewClient(config)

	return c, nil
}

// Run запускает подключение без ожидания готовности брокера или базы.
func (c *Client) Run(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return ErrClosed
	}

	if c.started {
		return nil
	}

	if ctx.Err() != nil {
		return ErrTimeout
	}

	c.started = true
	go c.worker()

	c.client.Connect()

	return nil
}

// Stop отменяет запрос и останавливает MQTT-клиент.
func (c *Client) Stop() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()

		return nil
	}

	c.closed = true
	c.ready = false
	close(c.stop)
	c.failLocked(ErrClosed)
	started := c.started
	c.mu.Unlock()

	c.client.Disconnect(0)
	c.setups.Wait()

	if started {
		<-c.done
	}

	return nil
}

// Shutdown завершает работу клиента.
func (c *Client) Shutdown(_ context.Context) error { return c.Stop() }

// Ready сообщает о подтверждённой подписке ответа.
func (c *Client) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.ready && !c.closed
}

// onConnect оформляет подписку текущего соединения.
func (c *Client) onConnect(client paho.Client) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()

		return
	}

	c.setups.Add(1)
	defer c.setups.Done()

	c.generation++
	generation := c.generation
	c.ready = false
	c.mu.Unlock()

	for {
		if c.subscribe(client, generation) {
			return
		}

		c.mu.Lock()
		current := !c.closed && c.generation == generation && client.IsConnectionOpen()
		c.mu.Unlock()

		if !current {
			return
		}

		timer := time.NewTimer(time.Second)
		select {
		case <-c.stop:
			timer.Stop()

			return
		case <-timer.C:
		}
	}
}

// onLost сбрасывает готовность и активный запрос.
func (c *Client) onLost(_ paho.Client, _ error) {
	c.mu.Lock()
	c.generation++
	c.ready = false
	c.failLocked(ErrUnavailable)
	c.mu.Unlock()
}

// failLocked завершает активное ожидание без блокировки MQTT router.
func (c *Client) failLocked(err error) {
	if c.pending != nil {
		select {
		case c.pending.result <- response{err: err}:
		default:
		}
	}
}

// receive копирует ограниченный ответ в очередь декодера.
func (c *Client) receive(_ paho.Client, message paho.Message) {
	if message.Retained() {
		return
	}

	c.mu.Lock()
	current := c.pending
	active := current != nil && !c.closed
	c.mu.Unlock()

	if !active {
		return
	}

	item := incoming{oversized: len(message.Payload()) > maxResponseBytes, pending: current}
	if !item.oversized {
		item.payload = append([]byte(nil), message.Payload()...)
	}

	select {
	case c.messages <- item:
	default:
	}
}

// worker декодирует ответы вне MQTT router.
func (c *Client) worker() {
	defer close(c.done)

	for {
		select {
		case <-c.stop:
			return
		case item := <-c.messages:
			c.mu.Lock()
			current := c.pending
			c.mu.Unlock()

			if current == nil || current != item.pending {
				continue
			}

			var (
				points    []Point
				errDecode error
			)
			if item.oversized {
				errDecode = ErrTooManyData
			} else {
				points, errDecode = decode(item.payload, current.id, current.query)
			}

			if errors.Is(errDecode, errUnrelated) {
				continue
			}

			c.mu.Lock()
			if c.pending == current {
				select {
				case current.result <- response{points: points, err: errDecode}:
				default:
				}
			}
			c.mu.Unlock()
		}
	}
}

// subscribe подтверждает успешный SUBACK текущего соединения.
func (c *Client) subscribe(client paho.Client, generation uint64) bool {
	c.mu.Lock()
	if c.closed || c.generation != generation || !client.IsConnectionOpen() {
		c.mu.Unlock()

		return false
	}

	token := client.Subscribe(c.topic+"/reply", 1, c.receive)
	c.mu.Unlock()

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()

	select {
	case <-c.stop:
		return false
	case <-timer.C:
		return false
	case <-token.Done():
	}

	if token.Error() != nil {
		return false
	}

	subscription, ok := token.(*paho.SubscribeToken)
	if !ok || subscription.Result()[c.topic+"/reply"] == 0x80 {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || c.generation != generation || !client.IsConnectionOpen() {
		return false
	}

	c.ready = true

	return true
}
