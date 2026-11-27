package main

import (
	"context"

	"github.com/Format-C-eft/wb-telegram/internal/bot"
	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// serviceHandlers связывает внешние события с ядром после сборки компонентов.
type serviceHandlers struct {
	core ServiceCore
}

// write передаёт ядру запись в контрол отправки.
func (h *serviceHandlers) write(controlID string, payload []byte) {
	if controlID == bot.SendControl {
		h.core.HandleSend(payload)
	}
}

// message передаёт ядру входящую команду.
func (h *serviceHandlers) message(ctx context.Context, msg telegram.Message) {
	h.core.HandleMessage(ctx, msg)
}

// callback передаёт ядру нажатие кнопки.
func (h *serviceHandlers) callback(ctx context.Context, query telegram.Callback) {
	h.core.HandleCallback(ctx, query)
}

// widget передаёт ядру запрос виджета.
func (h *serviceHandlers) widget(ctx context.Context, query telegram.WidgetQuery) {
	h.core.HandleWidgetQuery(ctx, query)
}

// mqtt передаёт состояние подключения к MQTT.
func (h *serviceHandlers) mqtt(connected bool) {
	h.core.ReportMQTT(connected)
}

// connection передаёт состояние подключения к Telegram.
func (h *serviceHandlers) connection(connected bool, reason string) {
	h.core.ReportTelegram(connected, reason)
}

// delivery передаёт результат отправки сообщения.
func (h *serviceHandlers) delivery(err error) {
	h.core.ReportDelivery(err)
}
