package bot

import (
	"reflect"
	"testing"

	"github.com/Format-C-eft/wb-telegram/internal/wbdevice"
)

// TestControls проверяет набор контролов устройства по конфигу.
func TestControls(t *testing.T) {
	t.Parallel()

	got := Controls(testConfig())
	want := []wbdevice.Control{
		{ID: "send", Meta: wbdevice.ControlMeta{Type: "text", Title: map[string]string{"en": "Message", "ru": "Сообщение"}, Order: 1}},
		{ID: "cmd_gate", Meta: wbdevice.ControlMeta{Type: "text", Title: map[string]string{"en": "/gate", "ru": "Открыть ворота"}, Order: 2, Readonly: true}, Initial: "{}", Volatile: true},
		{ID: "cmd_boiler", Meta: wbdevice.ControlMeta{Type: "text", Title: map[string]string{"en": "/boiler", "ru": "Состояние котла"}, Order: 3, Readonly: true}, Initial: "{}", Volatile: true},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Controls = %+v\nwant %+v", got, want)
	}
}
