package telegram

import (
	"context"
	"errors"
	"log/slog"
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
	// pollTimeout — таймаут long polling getUpdates.
	pollTimeout = 30 * time.Second
	// httpTimeout — таймаут HTTP-запроса, больше pollTimeout.
	httpTimeout = 35 * time.Second
)

// Message — входящее текстовое сообщение.
type Message struct {
	UpdateID int64
	ChatID   int64
	Text     string
	Date     time.Time
}

// Command — пункт меню команд Telegram.
type Command struct {
	Name        string
	Description string
}

// Handler обрабатывает входящее сообщение.
type Handler func(ctx context.Context, msg Message)

// Client — Telegram-клиент: long polling, меню команд, очередь отправки с лимитами.
type Client struct {
	api     *bot.Bot
	token   string
	options options
	queue   *queue
	limiter *limiter

	failing atomic.Bool
	fatal   atomic.Bool

	// fatalReported — onDelivery(ErrUnauthorized) уже отправлен; только для цикла отправки.
	fatalReported bool
	// lostInFlight — цикл отправки остановлен посреди доставки сообщения; пишется только циклом
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
		bot.WithHTTPClient(pollTimeout, &http.Client{Timeout: httpTimeout}),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message"}),
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

// Shutdown останавливает polling и досылает очередь, пока не истечёт ctx; что не успело уйти,
// выбрасывается с одним предупреждением в журнале (без текстов), ошибкой это не считается.
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

	// Истёкшая досылка — штатная остановка, а не ошибка: остаток очереди выбрасывается.
	if lost := c.undelivered(); lost > 0 {
		slog.Warn("Telegram: остановка, недоставленные сообщения выброшены", "count", lost)
	}

	return nil
}

// undelivered возвращает число сообщений, не доставленных к остановке цикла отправки:
// оставшиеся в очереди и прерванное посреди доставки. Вызывать после закрытия sendDone.
func (c *Client) undelivered() int {
	lost := c.queue.len()

	if c.lostInFlight {
		lost++
	}

	return lost
}

// Stop останавливает клиента с таймаутом досылки 5 секунд.
func (c *Client) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return c.Shutdown(ctx)
}

// dispatch оборачивает обработчик: только текстовые сообщения, апдейт — признак живой связи.
func (c *Client) dispatch(handler Handler) bot.HandlerFunc {
	return func(ctx context.Context, _ *bot.Bot, update *models.Update) {
		c.markHealthy()

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

// onPollError обрабатывает ошибки polling: 401 — фатально, отмена и потеря апдейтов при
// остановке игнорируются, ошибки разбора пишутся коротко (без текста сообщения), остальное — сбой связи.
func (c *Client) onPollError(err error) {
	text := err.Error()

	switch {
	case errors.Is(err, bot.ErrorUnauthorized):
		c.reportFatal(translate("poll", err, c.mask))
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
	case kindTransient, kindRateLimited:
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
	if c.failing.CompareAndSwap(false, true) {
		slog.Warn("Telegram: ошибка связи, повторяю в фоне", "err", message)

		return
	}

	slog.Debug("Telegram: ошибка связи продолжается", "err", message)
}

// markHealthy пишет в журнал восстановление связи после серии ошибок.
func (c *Client) markHealthy() {
	if c.failing.CompareAndSwap(true, false) {
		slog.Info("Telegram: связь восстановлена")
	}
}

// mask заменяет токен в тексте на ***.
func (c *Client) mask(text string) string {
	return strings.ReplaceAll(text, c.token, "***")
}
