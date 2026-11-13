package bot

import (
	"context"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/chart"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// Publisher — то, что ядру нужно от MQTT-устройства.
type Publisher interface {
	SetValue(ctx context.Context, controlID, value string) error
	SetError(ctx context.Context, controlID, flags string) error
	Connected() bool
}

// Sender — то, что ядру нужно от Telegram-клиента.
type Sender interface {
	Send(chatID int64, text string) error
	SendDelivery(chatID int64, delivery telegram.Delivery) error
	SendEphemeral(ctx context.Context, queryID string, delivery telegram.Delivery) error
	AnswerCallback(ctx context.Context, queryID, text string, alert bool) error
	ClearKeyboard(ctx context.Context, chatID int64, messageID int) error
	AnswerWidgetQuery(ctx context.Context, queryID, data string) error
	EditWidgetData(ctx context.Context, query telegram.WidgetQuery, data string) error
	SetCommands(ctx context.Context, chatID int64, commands []telegram.Command) error
	Stats() telegram.Stats
}

// ChartBuilder строит изображение истории по запросу правила.
type ChartBuilder interface {
	Build(ctx context.Context, spec chart.Spec, end time.Time) (chart.Result, error)
}
