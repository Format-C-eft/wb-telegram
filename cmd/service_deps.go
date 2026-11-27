package main

import (
	"context"

	"github.com/Format-C-eft/wb-telegram/internal/config"
	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

//go:generate ../bin/mockgen -source=service_deps.go -destination=mock/service_mock.go -package=mock

// DevicePurger убирает MQTT-устройство после отключения бота.
type DevicePurger interface {
	PurgeDevice(broker string)
}

// ServiceBackend предоставляет конфигурацию и запуск сервиса.
type ServiceBackend interface {
	DevicePurger
	LoadConfig(path string) (*config.Config, error)
	ReadToken(path string) (string, error)
	RunBot(ctx context.Context, cfg *config.Config, token, broker, apiURL string) int
}

// ApplicationRunner запускает приложение до его остановки.
type ApplicationRunner interface {
	Run(context.Context) error
}

// ServiceCore получает события внешних транспортов.
type ServiceCore interface {
	HandleSend([]byte)
	HandleMessage(context.Context, telegram.Message)
	HandleCallback(context.Context, telegram.Callback)
	HandleWidgetQuery(context.Context, telegram.WidgetQuery)
	ReportMQTT(bool)
	ReportTelegram(bool, string)
	ReportDelivery(error)
}

// localService подключает зависимости установленного сервиса.
type localService struct{}

// LoadConfig читает конфигурацию сервиса.
func (localService) LoadConfig(path string) (*config.Config, error) {
	return config.Load(path)
}

// ReadToken читает токен из защищённого файла.
func (localService) ReadToken(path string) (string, error) {
	return config.NewTokenStore(path).Read()
}

// RunBot запускает компоненты сервиса.
func (localService) RunBot(ctx context.Context, cfg *config.Config, token, broker, apiURL string) int {
	return runBot(ctx, cfg, token, broker, apiURL)
}

// PurgeDevice убирает устройство выключенного бота.
func (localService) PurgeDevice(broker string) {
	purgeDevice(broker)
}
