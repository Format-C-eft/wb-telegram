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
	chatID    int64
	command   string
	createdAt time.Time
	answered  bool
	stopTimer func() bool
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

	mu      sync.Mutex
	pending map[string]*pendingCall
	cancel  context.CancelFunc
	stopped bool

	// deliveryMu упорядочивает смену и публикацию флага ошибки доставки контрола send.
	deliveryMu      sync.Mutex
	deliveryFailing bool

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

	return &Bot{
		cfg:       cfg,
		publisher: publisher,
		sender:    sender,
		options:   o,
		pending:   map[string]*pendingCall{},
		done:      make(chan struct{}),
	}, nil
}

// Run в фоне устанавливает каждому пользователю меню доступных ему команд; не блокирует.
// Повторный Run и Run после Stop возвращают ErrInvalidState.
func (b *Bot) Run(ctx context.Context) error {
	b.mu.Lock()

	if b.cancel != nil || b.stopped {
		b.mu.Unlock()

		return &Error{kind: kindInvalidState, Op: "run", Message: "bot is already started or stopped"}
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	b.cancel = cancel

	b.mu.Unlock()

	go func() {
		defer close(b.done)

		for _, user := range b.cfg.Users {
			if runCtx.Err() != nil {
				return
			}

			b.syncUserCommands(runCtx, user)
		}
	}()

	return nil
}

// Stop окончательно останавливает ядро: прекращает установку меню, останавливает таймеры
// ожидания ответов и больше не принимает команды. Повторный Stop безопасен.
func (b *Bot) Stop() error {
	b.mu.Lock()

	b.stopped = true
	cancel := b.cancel

	for _, call := range b.pending {
		call.stop()
	}

	b.mu.Unlock()

	if cancel != nil {
		cancel()
		<-b.done
	}

	return nil
}

// isStopped сообщает, вызван ли Stop.
func (b *Bot) isStopped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.stopped
}

// syncUserCommands устанавливает меню команд пользователю, повторяя при временных ошибках.
func (b *Bot) syncUserCommands(ctx context.Context, user config.User) {
	commands := toTelegramCommands(b.cfg.CommandsFor(user.Name))

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

// deliver ставит сообщение в очередь отправки.
// Переполнение очереди клиент уже сообщил через onDelivery и журнал, а новое сообщение поставил,
// поэтому здесь оно не повторяется; прочие ошибки (клиент остановлен) только пишутся в журнал.
func (b *Bot) deliver(chatID int64, text string) {
	errSend := b.sender.Send(chatID, text)
	if errSend == nil || errors.Is(errSend, telegram.ErrQueueFull) {
		return
	}

	slog.Error("Telegram: сообщение не поставлено в очередь", "chat_id", chatID, "err", errSend)
}
