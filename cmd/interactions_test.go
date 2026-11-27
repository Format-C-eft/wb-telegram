package main

import (
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/cmd/mock"
	"github.com/Format-C-eft/wb-telegram/internal/bot"
	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// TestServiceWrite проверяет маршрутизацию записей MQTT в ядро.
func TestServiceWrite(t *testing.T) {
	t.Parallel()

	core := mock.NewMockServiceCore(gomock.NewController(t))
	handlers := serviceHandlers{core: core}
	payload := []byte(`{"text":"Открываю","reply_to":"42"}`)
	core.EXPECT().HandleSend(payload)

	handlers.write("cmd_gate", payload)
	handlers.write(bot.SendControl, payload)
}

// TestServiceTelegramHandlers проверяет передачу команд и интерактивных событий ядру.
func TestServiceTelegramHandlers(t *testing.T) {
	t.Parallel()

	core := mock.NewMockServiceCore(gomock.NewController(t))
	handlers := serviceHandlers{core: core}
	ctx := t.Context()

	message := telegram.Message{UpdateID: 42, ChatID: 100, Text: "/gate 22"}
	callback := telegram.Callback{UpdateID: 43, QueryID: "query-43", FromID: 100, ChatID: 100, MessageID: 77, Data: "selection", Accessible: true}
	widget := telegram.WidgetQuery{UpdateID: 44, QueryID: "widget-44", ChatID: 100, MessageID: 77, Data: "refresh"}

	core.EXPECT().HandleMessage(ctx, message)
	core.EXPECT().HandleCallback(ctx, callback)
	core.EXPECT().HandleWidgetQuery(ctx, widget)

	handlers.message(ctx, message)
	handlers.callback(ctx, callback)
	handlers.widget(ctx, widget)
}

// TestServiceConnectionHandlers проверяет передачу состояния транспортов ядру.
func TestServiceConnectionHandlers(t *testing.T) {
	t.Parallel()

	core := mock.NewMockServiceCore(gomock.NewController(t))
	handlers := serviceHandlers{core: core}
	errDelivery := errors.New("delivery failed")

	core.EXPECT().ReportMQTT(true)
	core.EXPECT().ReportMQTT(false)
	core.EXPECT().ReportTelegram(false, "connection lost")
	core.EXPECT().ReportTelegram(true, "")
	core.EXPECT().ReportDelivery(errDelivery)
	core.EXPECT().ReportDelivery(nil)

	handlers.mqtt(true)
	handlers.mqtt(false)
	handlers.connection(false, "connection lost")
	handlers.connection(true, "")
	handlers.delivery(errDelivery)
	handlers.delivery(nil)
}
