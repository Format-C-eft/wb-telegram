// Package bot реализует команды и доставку сообщений wb-telegram.
package bot

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/config"
	"github.com/Format-C-eft/wb-telegram/internal/retry"
	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

const (
	// DeviceID — идентификатор MQTT-устройства бота.
	DeviceID = "telegram_bot"
	// Driver — имя драйвера в meta устройства.
	Driver = "wb-telegram"
	// SendControl — контрол, через который правила шлют сообщения.
	SendControl = "send"
	// CommandControlPrefix — префикс контролов команд.
	CommandControlPrefix = "cmd_"
)

// pendingCall — вызов команды, ожидающий ответа правила.
type pendingCall struct {
	chatID          int64
	command         string
	createdAt       time.Time
	replyDeadline   time.Time
	source          commandSource
	ephemeralUsed   bool
	answered        bool
	answeredAt      time.Time
	timeoutNotified bool
	stopTimer       func() bool
}

// commandSource хранит адрес краткоживущего ответа Telegram.
type commandSource struct {
	callbackID string
	widget     *telegram.WidgetQuery
}

// stop останавливает таймер ожидания ответа, если он запущен.
func (c *pendingCall) stop() {
	if c.stopTimer != nil {
		c.stopTimer()
	}
}

// Bot — ядро: проверяет доступ, публикует команды, доставляет сообщения правил.
type Bot struct {
	cfg       *config.Config
	publisher Publisher
	sender    Sender
	options   options

	mu              sync.Mutex
	pending         map[string]*pendingCall
	cancel          context.CancelFunc
	stopped         bool
	operationCtx    context.Context
	operationCancel context.CancelFunc
	operations      sync.WaitGroup
	stopDone        chan struct{}
	panels          map[string]*buttonPanel
	buttons         map[string]buttonAction
	chartJobs       chan chartJob
	chartWorkers    sync.WaitGroup
	chartBusy       map[int64]bool
	chartSeen       map[string]chartSeenEntry
	shortReplies    []shortReply
	shortWorkers    int
	shortActive     map[widgetMessage]bool
	callbackActive  map[string]bool
	commandJobs     []commandJob
	commandDone     chan struct{}

	// deliveryMu защищает флаг доставки и его фоновую публикацию.
	deliveryMu      sync.Mutex
	deliveryFailing bool
	deliveryDone    chan struct{}

	telegramReportMu sync.Mutex
	// statsMu защищает stats; берётся после mu, никогда наоборот.
	statsMu sync.Mutex
	stats   serviceStats

	done chan struct{}
}

// New создаёт ядро бота.
func New(cfg *config.Config, publisher Publisher, sender Sender, opts ...Option) (*Bot, error) {
	if cfg == nil || publisher == nil || sender == nil {
		return nil, &Error{kind: kindInvalidArgument, Op: "new", Message: "config, publisher and sender are required"}
	}

	o := defaultOptions()

	for _, opt := range opts {
		opt(&o)
	}

	errValidate := o.Validate()
	if errValidate != nil {
		return nil, errValidate
	}

	operationCtx, operationCancel := context.WithCancel(context.Background())

	return &Bot{
		cfg:          cfg,
		publisher:    publisher,
		sender:       sender,
		options:      o,
		pending:      map[string]*pendingCall{},
		stats:        serviceStats{startedAt: o.now()},
		done:         make(chan struct{}),
		operationCtx: operationCtx, operationCancel: operationCancel, stopDone: make(chan struct{}),
		panels: make(map[string]*buttonPanel), buttons: make(map[string]buttonAction),
		chartJobs: make(chan chartJob, 8), chartBusy: make(map[int64]bool), chartSeen: make(map[string]chartSeenEntry),
		shortActive: make(map[widgetMessage]bool), callbackActive: make(map[string]bool),
	}, nil
}

// Run запускает фоновые задачи и установку меню команд.
func (b *Bot) Run(ctx context.Context) error {
	b.mu.Lock()

	if b.cancel != nil || b.stopped {
		b.mu.Unlock()

		return &Error{kind: kindInvalidState, Op: "run", Message: "bot is already started or stopped"}
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	b.cancel = cancel
	b.operations.Add(1)

	if b.options.chartBuilder != nil {
		b.chartWorkers.Add(1)
	}

	b.mu.Unlock()
	defer b.operations.Done()

	if b.options.chartBuilder != nil {
		go b.chartLoop()
	}

	go func() {
		defer close(b.done)

		for _, user := range b.cfg.Users {
			if runCtx.Err() != nil {
				return
			}

			b.syncUserCommands(runCtx, user)
		}
	}()

	b.notifyService("\U0001F916 wb-telegram " + b.options.version + " запущен")

	return nil
}

// Stop завершает операции ядра и освобождает его состояние.
func (b *Bot) Stop() error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		<-b.stopDone

		return nil
	}

	b.stopped = true
	b.shortReplies = nil
	b.commandJobs = nil
	cancel := b.cancel
	b.operationCancel()

	for _, call := range b.pending {
		call.stop()
	}
	b.mu.Unlock()

	if cancel != nil {
		cancel()
		<-b.done
	}

	b.operations.Wait()
	b.chartWorkers.Wait()

	b.statsMu.Lock()

	if b.stats.telegramStopTimer != nil {
		b.stats.telegramStopTimer()
	}

	b.statsMu.Unlock()

	b.mu.Lock()
	clear(b.pending)
	clear(b.panels)
	clear(b.buttons)
	clear(b.chartBusy)
	clear(b.chartSeen)
	b.mu.Unlock()
	close(b.stopDone)

	return nil
}

// beginOperation регистрирует операцию до закрытия приёма при остановке.
func (b *Bot) beginOperation() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stopped {
		return false
	}

	b.operations.Add(1)

	return true
}

// syncUserCommands устанавливает меню команд пользователю, повторяя при временных ошибках.
func (b *Bot) syncUserCommands(ctx context.Context, user config.User) {
	commands := toTelegramCommands(b.cfg.CommandsFor(user.Name))

	if statusAllowed(user) && len(commands) < config.MaxCommands {
		commands = append(commands, telegram.Command{Name: statusCommand, Description: statusDescription})
	}

	for attempt := 0; ; attempt++ {
		errSet := b.sender.SetCommands(ctx, user.ChatID, commands)
		if errSet == nil {
			return
		}

		if errors.Is(errSet, telegram.ErrUnauthorized) || errors.Is(errSet, telegram.ErrRejected) {
			slog.Warn("Telegram: меню команд не установлено", "user", user.Name, "err", errSet)

			return
		}

		slog.Debug("Telegram: повторю установку меню команд", "user", user.Name, "err", errSet)

		if !b.options.sleep(ctx, b.retryPause(attempt, errSet)) {
			return
		}
	}
}

// retryPause возвращает паузу перед повтором: экспоненциальную, но не меньше retry_after из ответа 429.
func (b *Bot) retryPause(attempt int, err error) time.Duration {
	pause := retry.Delay(attempt, b.options.retryBase, b.options.retryMax)

	var telegramErr *telegram.Error
	if errors.As(err, &telegramErr) && telegramErr.RetryAfter > pause {
		pause = telegramErr.RetryAfter
	}

	return pause
}

// toTelegramCommands переводит команды конфига в пункты меню Telegram.
func toTelegramCommands(commands []config.Command) []telegram.Command {
	out := make([]telegram.Command, 0, len(commands))

	for _, cmd := range commands {
		out = append(out, telegram.Command{Name: cmd.Name, Description: cmd.Description})
	}

	return out
}

// deliver ставит обычный текст в очередь отправки.
func (b *Bot) deliver(chatID int64, text string) {
	errSend := b.sender.Send(chatID, text)
	if errSend == nil || errors.Is(errSend, telegram.ErrQueueFull) {
		return
	}

	slog.Error("Telegram: сообщение не поставлено в очередь", "chat_id", chatID, "err", errSend)
}

// errorKind классифицирует ошибки пакета bot.
type errorKind int

const (
	kindUnknown errorKind = iota
	kindInvalidArgument
	kindBadPayload
	kindInvalidState
)

// Error - ошибка пакета bot.
type Error struct {
	kind    errorKind
	Op      string
	Message string
}

// Error возвращает текст ошибки.
func (e *Error) Error() string {
	if e.Op == "" {
		return "bot: " + e.Message
	}

	return "bot: " + e.Op + ": " + e.Message
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
	// ErrInvalidArgument сообщает о некорректном аргументе конструктора или опции.
	ErrInvalidArgument = &Error{kind: kindInvalidArgument, Message: "invalid argument"}
	// ErrBadPayload сообщает о некорректном сообщении от правил.
	ErrBadPayload = &Error{kind: kindBadPayload, Message: "bad payload"}
	// ErrInvalidState сообщает о вызове в неподходящем состоянии (повторный Run, Run после Stop).
	ErrInvalidState = &Error{kind: kindInvalidState, Message: "invalid state"}
)
