package bot

import (
	"github.com/Format-C-eft/wb-telegram/internal/config"
	"github.com/Format-C-eft/wb-telegram/internal/wbdevice"
)

// Controls возвращает контролы устройства: send и по контролу на каждую команду в порядке настроек.
// Контролы команд Volatile, send — нет: последнее сообщение правила остаётся видно в UI.
func Controls(cfg *config.Config) []wbdevice.Control {
	controls := []wbdevice.Control{{
		ID: SendControl,
		Meta: wbdevice.ControlMeta{
			Type:  wbdevice.ControlTypeText,
			Title: map[string]string{"en": "Message", "ru": "Сообщение"},
			Order: 1,
		},
	}}

	for index, cmd := range cfg.Commands {
		controls = append(controls, wbdevice.Control{
			ID: CommandControlPrefix + cmd.Name,
			Meta: wbdevice.ControlMeta{
				Type:     wbdevice.ControlTypeText,
				Title:    map[string]string{"en": "/" + cmd.Name, "ru": cmd.Description},
				Order:    index + 2,
				Readonly: true,
			},
			// Вызов команды одноразовый: после переподключения правило не должно получить его снова.
			Volatile: true,
		})
	}

	return controls
}
