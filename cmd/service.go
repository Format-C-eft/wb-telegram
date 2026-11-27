package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/app"
	"github.com/Format-C-eft/wb-telegram/internal/bot"
	"github.com/Format-C-eft/wb-telegram/internal/chart"
	"github.com/Format-C-eft/wb-telegram/internal/config"
	"github.com/Format-C-eft/wb-telegram/internal/telegram"
	"github.com/Format-C-eft/wb-telegram/internal/wb_history"
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
	return runServiceWithBackend(ctx, s, localService{})
}

// runServiceWithBackend выполняет запуск через зависимости сервиса.
func runServiceWithBackend(ctx context.Context, s serviceSettings, backend ServiceBackend) int {
	cfg, errLoad := backend.LoadConfig(s.configPath)
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
		backend.PurgeDevice(broker)

		return 0
	}

	token, errToken := backend.ReadToken(s.tokenPath)
	if errors.Is(errToken, config.ErrNoToken) {
		slog.Info("Токен не задан, задайте его в веб-интерфейсе")
		backend.PurgeDevice(broker)

		return 0
	}

	if errToken != nil {
		slog.Error("токен не прочитан", "err", errToken)

		return 1
	}

	return backend.RunBot(ctx, cfg, token, broker, s.apiURL)
}

// runBot собирает компоненты и запускает сервис.
func runBot(ctx context.Context, cfg *config.Config, token, broker, apiURL string) int {
	application, errApp := app.New()
	if errApp != nil {
		slog.Error("не удалось создать приложение", "err", errApp)

		return 1
	}

	handlers := &serviceHandlers{}

	device, errDevice := wbdevice.New(bot.DeviceID,
		wbdevice.WithBroker(broker),
		wbdevice.WithDriver(bot.Driver),
		wbdevice.WithTitle("Telegram Bot", "Telegram-бот"),
		wbdevice.WithControls(bot.Controls(cfg)...),
		wbdevice.WithWriteHandler(handlers.write),
		wbdevice.WithOnConnection(handlers.mqtt),
	)
	if errDevice != nil {
		slog.Error("не удалось создать MQTT-устройство", "err", errDevice)

		return 1
	}

	history, errHistory := wb_history.New(wb_history.WithBroker(broker))
	if errHistory != nil {
		slog.Error("не удалось создать клиент истории", "err", errHistory)

		return 1
	}

	builder, errBuilder := chart.New(history)
	if errBuilder != nil {
		slog.Error("не удалось создать построитель графиков", "err", errBuilder)

		return 1
	}

	telegramOptions := []telegram.Option{
		telegram.WithTimeouts(cfg.ConnectTimeout(), cfg.PollTimeout()),
		telegram.WithCallbackHandler(handlers.callback),
		telegram.WithWidgetQueryHandler(handlers.widget),
		telegram.WithOnFatal(application.Terminate),
		telegram.WithOnDelivery(handlers.delivery),
		telegram.WithOnConnection(handlers.connection),
	}

	if apiURL != "" {
		telegramOptions = append(telegramOptions, telegram.WithServerURL(apiURL))
	}

	client, errClient := telegram.New(token, handlers.message, telegramOptions...)
	if errClient != nil {
		slog.Error("не удалось создать Telegram-клиент", "err", errClient)

		return 1
	}

	core, errCore := bot.New(cfg, device, client, bot.WithVersion(version), bot.WithChartBuilder(builder))
	if errCore != nil {
		slog.Error("не удалось создать ядро бота", "err", errCore)

		return 1
	}

	handlers.core = core

	application.AddComponent(device, history, client, core)

	slog.Info("wb-telegram запущен", "version", version, "users", len(cfg.Users), "commands", len(cfg.Commands))

	return runApplication(ctx, application, broker, localService{})
}

// runApplication возвращает код завершения приложения и убирает устройство при отказе токена.
func runApplication(ctx context.Context, application ApplicationRunner, broker string, purger DevicePurger) int {
	errRun := application.Run(ctx)

	if errors.Is(errRun, telegram.ErrUnauthorized) {
		slog.Error("Telegram отклонил токен, задайте новый в веб-интерфейсе")
		purger.PurgeDevice(broker)

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
