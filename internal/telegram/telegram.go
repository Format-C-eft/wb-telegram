// Package telegram предоставляет транспорт Telegram Bot API.
package telegram

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/Format-C-eft/wb-telegram/internal/retry"
)

const (
	// requestMargin - запас таймаута HTTP-запроса сверх подключения и long polling.
	requestMargin = 10 * time.Second
	// tcpKeepAlive - период TCP keep-alive: соединение с Bot API переиспользуется между запросами.
	tcpKeepAlive = 30 * time.Second
	// maxPingInterval - после стольких секунд тишины на HTTP/2-соединении шлётся ping.
	maxPingInterval = 15 * time.Second
)

// Message - входящее текстовое сообщение.
type Message struct {
	UpdateID int64
	ChatID   int64
	Text     string
	Date     time.Time
}

// Command - пункт меню команд Telegram.
type Command struct {
	Name        string
	Description string
}

// Handler обрабатывает входящее сообщение.
type Handler func(ctx context.Context, msg Message)

// Client - Telegram-клиент: long polling, меню команд, очередь отправки с лимитами.
type Client struct {
	api     *bot.Bot
	token   string
	options options
	queue   *queue
	limiter *limiter

	failing atomic.Bool
	fatal   atomic.Bool

	// failMu защищает failSince и failErrors; берётся при смене состояния failing.
	failMu     sync.Mutex
	failSince  time.Time
	failErrors int

	// sent и dropped - счётчики доставленных и выброшенных сообщений для Stats.
	sent    atomic.Uint64
	dropped atomic.Uint64

	// fatalReported - onDelivery(ErrUnauthorized) уже отправлен; только для цикла отправки.
	fatalReported bool
	// lostInFlight - цикл отправки остановлен посреди доставки сообщения; пишется только циклом
	// отправки до закрытия sendDone, читается после него.
	lostInFlight bool

	mu          sync.Mutex
	started     bool
	closed      bool
	runCancel   context.CancelFunc
	sendStop    context.CancelFunc
	pollDone    chan struct{}
	sendDone    chan struct{}
	webhookDone chan struct{}
	control     chan struct{}
}

// New создаёт клиента; сетевых вызовов не делает.
func New(token string, handler Handler, opts ...Option) (*Client, error) {
	if strings.TrimSpace(token) == "" || handler == nil {
		return nil, &Error{kind: kindInvalidArgument, Op: "new", Message: "token and handler are required"}
	}

	o := defaultOptions()

	for _, opt := range opts {
		opt(&o)
	}

	errValidate := o.Validate()
	if errValidate != nil {
		return nil, errValidate
	}

	c := &Client{
		token:       token,
		control:     make(chan struct{}, 4),
		options:     o,
		queue:       newQueue(o.queueSize),
		limiter:     newLimiter(o.perChatInterval, o.globalInterval, time.Now),
		pollDone:    make(chan struct{}),
		sendDone:    make(chan struct{}),
		webhookDone: make(chan struct{}),
	}

	api, errNew := bot.New(token,
		bot.WithSkipGetMe(),
		bot.WithServerURL(o.serverURL),
		bot.WithHTTPClient(o.pollTimeout, &healthClient{inner: newHTTPClient(o.connectTimeout, o.pollTimeout), alive: c.markHealthy}),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "callback_query"}),
		bot.WithDefaultHandler(c.dispatch(handler)),
		bot.WithErrorsHandler(c.onPollError),
	)
	if errNew != nil {
		return nil, translate("new", errNew, c.mask)
	}

	c.api = api

	return c, nil
}

// Run запускает удаление webhook, long polling и цикл отправки; не блокирует.
func (c *Client) Run(ctx context.Context) error {
	c.mu.Lock()

	if c.closed {
		c.mu.Unlock()

		return &Error{kind: kindClosed, Op: "run", Message: "client is closed"}
	}

	if c.started {
		c.mu.Unlock()

		return &Error{kind: kindInvalidArgument, Op: "run", Message: "already started"}
	}

	runCtx, runCancel := context.WithCancel(context.WithoutCancel(ctx))
	sendCtx, sendStop := context.WithCancel(context.WithoutCancel(ctx))

	c.started = true
	c.runCancel = runCancel
	c.sendStop = sendStop

	c.mu.Unlock()

	go func() {
		defer close(c.webhookDone)

		c.deleteWebhook(runCtx)
	}()

	go func() {
		defer close(c.pollDone)

		c.api.Start(runCtx)
	}()

	go func() {
		defer close(c.sendDone)

		c.sendLoop(sendCtx)
	}()

	return nil
}

// SetCommands задаёт меню команд для конкретного чата (scope chat).
func (c *Client) SetCommands(ctx context.Context, chatID int64, commands []Command) error {
	botCommands := make([]models.BotCommand, 0, len(commands))

	for _, cmd := range commands {
		botCommands = append(botCommands, models.BotCommand{Command: cmd.Name, Description: cmd.Description})
	}

	_, errSet := c.api.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: botCommands,
		Scope:    &models.BotCommandScopeChat{ChatID: chatID},
	})
	if errSet != nil {
		return c.fail("set commands", errSet)
	}

	c.markHealthy()

	return nil
}

// Shutdown останавливает polling и досылает очередь до истечения контекста.
func (c *Client) Shutdown(ctx context.Context) error {
	c.mu.Lock()

	if c.closed {
		c.mu.Unlock()

		return nil
	}

	c.closed = true
	started := c.started
	runCancel := c.runCancel
	sendStop := c.sendStop

	c.mu.Unlock()

	c.queue.close()

	if !started {
		c.queue.release()

		return nil
	}

	runCancel()

	select {
	case <-c.sendDone:
	case <-ctx.Done():
		sendStop()
		<-c.sendDone
	}

	sendStop()

	<-c.pollDone
	<-c.webhookDone

	// Истёкшая досылка - штатная остановка, а не ошибка: остаток очереди выбрасывается.
	if lost := c.undelivered(); lost > 0 {
		slog.Warn("Telegram: остановка, недоставленные сообщения выброшены", "count", lost)
	}

	c.queue.release()

	return nil
}

// undelivered возвращает число недоставленных сообщений после остановки отправки.
func (c *Client) undelivered() int {
	lost := c.queue.len()

	if c.lostInFlight {
		lost++
	}

	return lost
}

// newHTTPClient создаёт HTTP-клиент с таймаутами и проверкой соединения.
func newHTTPClient(connect, poll time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: connect, KeepAlive: tcpKeepAlive}).DialContext
	transport.TLSHandshakeTimeout = connect
	transport.HTTP2 = &http.HTTP2Config{
		SendPingTimeout:  min(connect, maxPingInterval),
		PingTimeout:      connect,
		WriteByteTimeout: connect,
	}

	return &http.Client{
		Timeout:   connect + poll + requestMargin,
		Transport: transport,
	}
}

// Stop останавливает клиента с таймаутом досылки 5 секунд.
func (c *Client) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return c.Shutdown(ctx)
}

// dispatch оборачивает обработчик: только текстовые сообщения, апдейт - признак живой связи.
func (c *Client) dispatch(handler Handler) bot.HandlerFunc {
	return func(ctx context.Context, _ *bot.Bot, update *models.Update) {
		c.markHealthy()

		if update.CallbackQuery != nil {
			if c.options.onCallback != nil {
				c.options.onCallback(ctx, callbackFromUpdate(update))
			}

			return
		}

		if update.Message == nil || update.Message.Text == "" {
			return
		}

		handler(ctx, Message{
			UpdateID: update.ID,
			ChatID:   update.Message.Chat.ID,
			Text:     update.Message.Text,
			Date:     time.Unix(int64(update.Message.Date), 0),
		})
	}
}

// onPollError обрабатывает ошибки polling и состояние связи.
func (c *Client) onPollError(err error) {
	text := err.Error()

	var tooMany *bot.TooManyRequestsError

	switch {
	case errors.Is(err, bot.ErrorUnauthorized):
		c.reportFatal(translate("poll", err, c.mask))
	case errors.As(err, &tooMany):
		slog.Info("Telegram: слишком частые запросы, жду", "retry_after", time.Duration(tooMany.RetryAfter)*time.Second)
	case errors.Is(err, context.Canceled), strings.HasPrefix(text, "some updates lost"):
		return
	case strings.HasPrefix(text, "error decode update"):
		slog.Warn("Telegram: не удалось разобрать апдейт, пропущен")
	default:
		c.markFailing(c.mask(text))
	}
}

// deleteWebhook отключает webhook с повторами, чтобы работал long polling.
func (c *Client) deleteWebhook(ctx context.Context) {
	for attempt := 0; ; attempt++ {
		_, errDelete := c.api.DeleteWebhook(ctx, &bot.DeleteWebhookParams{})
		if errDelete == nil {
			c.markHealthy()

			return
		}

		failure := c.fail("delete webhook", errDelete)

		switch {
		case ctx.Err() != nil, errors.Is(failure, ErrUnauthorized):
			return
		case errors.Is(failure, ErrRejected):
			slog.Warn("Telegram: deleteWebhook отклонён, повторов не будет", "err", failure.Message)

			return
		}

		pause := max(failure.RetryAfter, retry.Delay(attempt, c.options.retryBase, c.options.retryMax))

		if !c.options.sleep(ctx, pause) {
			return
		}
	}
}

// fail переводит ошибку, сообщает о фатальной, отмечает сбой связи и возвращает переведённую.
func (c *Client) fail(op string, err error) *Error {
	translated := translate(op, err, c.mask)

	if errors.Is(err, context.Canceled) {
		return translated
	}

	switch translated.kind {
	case kindUnauthorized:
		c.reportFatal(translated)
	case kindRateLimited:
		slog.Info("Telegram: слишком частые запросы, жду", "op", op, "retry_after", translated.RetryAfter)
	case kindTransient:
		c.markFailing(translated.Error())
	}

	return translated
}

// reportFatal вызывает onFatal не более одного раза за жизнь клиента, из какой бы горутины ни пришла ошибка.
func (c *Client) reportFatal(err *Error) {
	if c.fatal.CompareAndSwap(false, true) {
		c.options.onFatal(err)
	}
}

// markFailing пишет в журнал только первую ошибку связи подряд.
func (c *Client) markFailing(message string) {
	c.failMu.Lock()

	c.failErrors++

	first := c.failing.CompareAndSwap(false, true)
	if first {
		c.failSince = time.Now()
	}

	c.failMu.Unlock()

	if first {
		slog.Warn("Telegram: ошибка связи, повторяю в фоне", "err", message)
		c.options.onConnection(false, message)

		return
	}

	slog.Debug("Telegram: ошибка связи продолжается", "err", message)
}

// markHealthy пишет в журнал восстановление связи после серии ошибок: сколько длилась и сколько ошибок подряд.
func (c *Client) markHealthy() {
	if !c.failing.Load() {
		return
	}

	c.failMu.Lock()

	recovered := c.failing.CompareAndSwap(true, false)
	since := c.failSince
	errorsInRow := c.failErrors
	c.failErrors = 0

	c.failMu.Unlock()

	if !recovered {
		return
	}

	slog.Info("Telegram: связь восстановлена", "после", time.Since(since).Round(time.Millisecond), "ошибок", errorsInRow)
	c.options.onConnection(true, "")
}

// Stats - счётчики клиента с момента запуска.
type Stats struct {
	// Sent - доставлено сообщений.
	Sent uint64
	// Dropped - выброшено сообщений: отклонены Telegram, переполнение очереди, отклонённый токен.
	Dropped uint64
	// Queued - сообщений в очереди сейчас.
	Queued int
	// Connected - связь с Bot API есть (последний запрос без ошибки связи).
	Connected bool
}

// Stats возвращает счётчики клиента.
func (c *Client) Stats() Stats {
	return Stats{
		Sent:      c.sent.Load(),
		Dropped:   c.dropped.Load(),
		Queued:    c.queue.len(),
		Connected: !c.failing.Load(),
	}
}

// mask заменяет токен в тексте на ***.
func (c *Client) mask(text string) string {
	return strings.ReplaceAll(text, c.token, "***")
}

// errorKind классифицирует ошибки пакета telegram.
type errorKind int

const (
	kindUnknown errorKind = iota
	kindUnauthorized
	kindRateLimited
	kindRejected
	kindTransient
	kindQueueFull
	kindClosed
	kindInvalidArgument
	kindBusy
)

// Error - ошибка пакета telegram; текст никогда не содержит токен.
type Error struct {
	kind       errorKind
	Op         string
	Message    string
	RetryAfter time.Duration
}

// Error возвращает текст ошибки.
func (e *Error) Error() string {
	if e.Op == "" {
		return "telegram: " + e.Message
	}

	return "telegram: " + e.Op + ": " + e.Message
}

// Is сравнивает ошибки по виду.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}

	return e.kind == t.kind
}

var (
	// ErrBusy сообщает о занятых управляющих запросах.
	ErrBusy = &Error{kind: kindBusy, Message: "control requests busy"}
	// ErrUnauthorized сообщает, что Telegram отклонил токен.
	ErrUnauthorized = &Error{kind: kindUnauthorized, Message: "unauthorized"}
	// ErrRateLimited сообщает об ответе 429; RetryAfter - сколько ждать.
	ErrRateLimited = &Error{kind: kindRateLimited, Message: "too many requests"}
	// ErrRejected сообщает об окончательном отказе (400/403/404), повтор бессмыслен.
	ErrRejected = &Error{kind: kindRejected, Message: "rejected"}
	// ErrTransient сообщает о временной ошибке (сеть, 5xx), можно повторить.
	ErrTransient = &Error{kind: kindTransient, Message: "temporary failure"}
	// ErrQueueFull сообщает, что очередь переполнена и самое старое сообщение выброшено.
	ErrQueueFull = &Error{kind: kindQueueFull, Message: "queue is full, oldest message dropped"}
	// ErrClosed сообщает, что клиент остановлен.
	ErrClosed = &Error{kind: kindClosed, Message: "client is closed"}
	// ErrInvalidArgument сообщает о некорректном аргументе или опции.
	ErrInvalidArgument = &Error{kind: kindInvalidArgument, Message: "invalid argument"}
)

// translate переводит ошибку go-telegram/bot в *Error пакета с замаскированным текстом.
func translate(op string, err error, mask func(string) string) *Error {
	message := mask(err.Error())

	var tooMany *bot.TooManyRequestsError

	switch {
	case errors.As(err, &tooMany):
		return &Error{kind: kindRateLimited, Op: op, Message: message, RetryAfter: time.Duration(tooMany.RetryAfter) * time.Second}
	case errors.Is(err, bot.ErrorUnauthorized):
		return &Error{kind: kindUnauthorized, Op: op, Message: message}
	case errors.Is(err, bot.ErrorForbidden), errors.Is(err, bot.ErrorBadRequest), errors.Is(err, bot.ErrorNotFound):
		return &Error{kind: kindRejected, Op: op, Message: message}
	default:
		return &Error{kind: kindTransient, Op: op, Message: message}
	}
}
