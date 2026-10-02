package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/app"
	"github.com/Format-C-eft/wb-telegram/internal/bot"
	"github.com/Format-C-eft/wb-telegram/internal/config"
	"github.com/Format-C-eft/wb-telegram/internal/telegram"
	"github.com/Format-C-eft/wb-telegram/internal/wbdevice"
)

// purgeWindow — окно сбора retained-топиков при уборке устройства.
const purgeWindow = time.Second

// serviceSettings — настройки режима сервиса.
type serviceSettings struct {
	configPath string
	tokenPath  string
	broker     string
	// apiURL — адрес Bot API; пусто — api.telegram.org (задают только тесты).
	apiURL string
	// logLevel — уровень глобального журнала; если задан, debug из конфига включает отладку.
	logLevel *slog.LevelVar
}

// runService запускает бота и возвращает код выхода (таблица кодов — README, раздел «Ошибки и диагностика»).
func runService(ctx context.Context, s serviceSettings) int {
	cfg, errLoad := config.Load(s.configPath)
	if errLoad != nil {
		slog.Error("конфиг не прочитан, исправьте его в веб-интерфейсе", "path", s.configPath, "err", errLoad)

		return 1
	}

	if cfg.Debug && s.logLevel != nil {
		s.logLevel.Set(slog.LevelDebug)
	}

	broker := s.broker
	if broker == "" {
		broker = wbdevice.DefaultBroker(wbdevice.MosquittoSocket)
	}

	if !cfg.Enabled {
		slog.Info("Бот выключен в настройках")
		purgeDevice(broker)

		return 0
	}

	token, errToken := config.NewTokenStore(s.tokenPath).Read()
	if errors.Is(errToken, config.ErrNoToken) {
		slog.Info("Токен не задан, задайте его в веб-интерфейсе")
		purgeDevice(broker)

		return 0
	}

	if errToken != nil {
		slog.Error("токен не прочитан", "err", errToken)

		return 1
	}

	return runBot(ctx, cfg, token, broker, s.apiURL)
}

// runBot собирает компоненты (wbdevice → telegram → bot), запускает их и переводит результат в код выхода.
// Остановка — в обратном порядке: ядро, досылка очереди Telegram (до 5 с), устройство с meta/error "r".
func runBot(ctx context.Context, cfg *config.Config, token, broker, apiURL string) int {
	application, errApp := app.New()
	if errApp != nil {
		slog.Error("не удалось создать приложение", "err", errApp)

		return 1
	}

	// core создаётся последним (ему нужны устройство и клиент), а обработчики устройства и клиента
	// ссылаются на него; вызываются они только после application.Run, когда core уже задан.
	var core *bot.Bot

	device, errDevice := wbdevice.New(bot.DeviceID,
		wbdevice.WithBroker(broker),
		wbdevice.WithDriver(bot.Driver),
		wbdevice.WithTitle("Telegram Bot", "Telegram-бот"),
		wbdevice.WithControls(bot.Controls(cfg)...),
		wbdevice.WithWriteHandler(func(controlID string, payload []byte) {
			if controlID == bot.SendControl {
				core.HandleSend(payload)
			}
		}),
	)
	if errDevice != nil {
		slog.Error("не удалось создать MQTT-устройство", "err", errDevice)

		return 1
	}

	telegramOptions := []telegram.Option{
		telegram.WithOnFatal(application.Terminate),
		telegram.WithOnDelivery(func(err error) { core.ReportDelivery(err) }),
	}

	if apiURL != "" {
		telegramOptions = append(telegramOptions, telegram.WithServerURL(apiURL))
	}

	client, errClient := telegram.New(token, func(handlerCtx context.Context, msg telegram.Message) {
		core.HandleMessage(handlerCtx, msg)
	}, telegramOptions...)
	if errClient != nil {
		slog.Error("не удалось создать Telegram-клиент", "err", errClient)

		return 1
	}

	var errCore error

	core, errCore = bot.New(cfg, device, client)
	if errCore != nil {
		slog.Error("не удалось создать ядро бота", "err", errCore)

		return 1
	}

	application.AddComponent(device, client, core)

	slog.Info("wb-telegram запущен", "version", version, "users", len(cfg.Users), "commands", len(cfg.Commands))

	errRun := application.Run(ctx)

	if errors.Is(errRun, telegram.ErrUnauthorized) {
		slog.Error("Telegram отклонил токен, задайте новый в веб-интерфейсе")
		purgeDevice(broker)

		return 0
	}

	if errRun != nil {
		slog.Error("сервис остановлен с ошибкой", "err", errRun)

		return 1
	}

	slog.Info("wb-telegram остановлен")

	return 0
}

// purgeDevice убирает устройство бота из MQTT; ошибка уборки только пишется в журнал.
func purgeDevice(broker string) {
	errPurge := wbdevice.Purge(broker, bot.DeviceID, purgeWindow)
	if errPurge != nil {
		slog.Warn("не удалось убрать устройство из MQTT", "err", errPurge)
	}
}
