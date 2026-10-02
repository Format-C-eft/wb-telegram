package bot

import (
	"context"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// Publisher — то, что ядру нужно от MQTT-устройства.
type Publisher interface {
	SetValue(controlID, value string) error
	SetError(controlID, flags string) error
	Connected() bool
}

// Sender — то, что ядру нужно от Telegram-клиента.
type Sender interface {
	Send(chatID int64, text string) error
	SetCommands(ctx context.Context, chatID int64, commands []telegram.Command) error
}
