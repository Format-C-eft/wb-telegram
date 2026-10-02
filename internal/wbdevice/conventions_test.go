package wbdevice

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultBroker проверяет выбор unix-сокета mosquitto или TCP.
func TestDefaultBroker(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(t.TempDir(), "mosquitto.sock")

	if got := DefaultBroker(socket); got != "tcp://localhost:1883" {
		t.Fatalf("without socket = %q", got)
	}

	errWrite := os.WriteFile(socket, nil, 0o600)
	if errWrite != nil {
		t.Fatalf("WriteFile: %v", errWrite)
	}

	if got := DefaultBroker(socket); got != "unix://"+socket {
		t.Fatalf("with socket = %q", got)
	}
}

// TestTopics проверяет построение топиков по MQTT Conventions.
func TestTopics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "device meta", got: deviceMetaTopic("telegram_bot"), want: "/devices/telegram_bot/meta"},
		{name: "device error", got: deviceErrorTopic("telegram_bot"), want: "/devices/telegram_bot/meta/error"},
		{name: "control", got: controlTopic("telegram_bot", "send"), want: "/devices/telegram_bot/controls/send"},
		{name: "control meta", got: controlMetaTopic("telegram_bot", "send"), want: "/devices/telegram_bot/controls/send/meta"},
		{name: "control error", got: controlErrorTopic("telegram_bot", "send"), want: "/devices/telegram_bot/controls/send/meta/error"},
		{name: "control on", got: controlOnTopic("telegram_bot", "send"), want: "/devices/telegram_bot/controls/send/on"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.got != tt.want {
				t.Fatalf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

// TestControlIDFromTopic проверяет извлечение ID контрола из топика.
func TestControlIDFromTopic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		topic  string
		wantID string
		wantOK bool
	}{
		{topic: "/devices/telegram_bot/controls/cmd_gate", wantID: "cmd_gate", wantOK: true},
		{topic: "/devices/telegram_bot/controls/cmd_gate/meta/error", wantID: "cmd_gate", wantOK: true},
		{topic: "/devices/telegram_bot/meta", wantOK: false},
		{topic: "/devices/other/controls/x", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			t.Parallel()

			id, ok := controlIDFromTopic("telegram_bot", tt.topic)
			if id != tt.wantID || ok != tt.wantOK {
				t.Fatalf("got %q, %v; want %q, %v", id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}
