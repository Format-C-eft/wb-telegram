package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// validConfig возвращает корректный конфиг для тестов.
func validConfig() *Config {
	return &Config{
		Enabled:         true,
		ConnectTimeoutS: 30,
		PollTimeoutS:    30,
		ReplyTimeoutS:   10,
		Users: []User{
			{Name: "Супруга", ChatID: 100, Notifications: true},
			{Name: "Ребёнок", ChatID: 200},
		},
		Commands: []Command{
			{Name: "gate", Description: "Открыть ворота", Users: []string{}},
			{Name: "boiler", Description: "Котёл", Users: []string{"Супруга"}},
		},
	}
}

// TestValidate проверяет все правила валидации конфига.
func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{name: "valid", mutate: func(*Config) {}},
		{name: "timeout too small", mutate: func(c *Config) { c.ReplyTimeoutS = 0 }, wantErr: "reply_timeout_s"},
		{name: "timeout too big", mutate: func(c *Config) { c.ReplyTimeoutS = 301 }, wantErr: "reply_timeout_s"},
		{name: "connect timeout too small", mutate: func(c *Config) { c.ConnectTimeoutS = 4 }, wantErr: "connect_timeout_s"},
		{name: "connect timeout too big", mutate: func(c *Config) { c.ConnectTimeoutS = 121 }, wantErr: "connect_timeout_s"},
		{name: "poll timeout too small", mutate: func(c *Config) { c.PollTimeoutS = -1 }, wantErr: "poll_timeout_s"},
		{name: "poll timeout too big", mutate: func(c *Config) { c.PollTimeoutS = 51 }, wantErr: "poll_timeout_s"},
		{name: "empty user name", mutate: func(c *Config) { c.Users[0].Name = " " }, wantErr: "имя пользователя"},
		{name: "duplicate user name", mutate: func(c *Config) { c.Users[1].Name = "Супруга" }, wantErr: "повторяется имя пользователя"},
		{name: "zero chat id", mutate: func(c *Config) { c.Users[0].ChatID = 0 }, wantErr: "chat_id"},
		{name: "duplicate chat id", mutate: func(c *Config) { c.Users[1].ChatID = 100 }, wantErr: "повторяется chat_id"},
		{name: "bad command name", mutate: func(c *Config) { c.Commands[0].Name = "Gate!" }, wantErr: "имя команды"},
		{name: "reserved command", mutate: func(c *Config) { c.Commands[0].Name = "help" }, wantErr: "зарезервирована"},
		{name: "duplicate command", mutate: func(c *Config) { c.Commands[1].Name = "gate" }, wantErr: "повторяется команда"},
		{name: "empty description", mutate: func(c *Config) { c.Commands[0].Description = "" }, wantErr: "описание"},
		{name: "long description", mutate: func(c *Config) { c.Commands[0].Description = strings.Repeat("я", 257) }, wantErr: "описание"},
		{name: "unknown user in command", mutate: func(c *Config) { c.Commands[1].Users = []string{"Гость"} }, wantErr: "неизвестный пользователь"},
		{name: "duplicate user in command", mutate: func(c *Config) { c.Commands[1].Users = []string{"Супруга", "Супруга"} }, wantErr: "повторяется пользователь"},
		{name: "too many commands", mutate: func(c *Config) {
			c.Commands = nil

			for i := range MaxCommands + 1 {
				c.Commands = append(c.Commands, Command{Name: fmt.Sprintf("c%03d", i), Description: "d"})
			}
		}, wantErr: "не больше 100"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			tt.mutate(cfg)

			errValidate := cfg.Validate()
			if tt.wantErr == "" {
				if errValidate != nil {
					t.Fatalf("Validate: %v", errValidate)
				}

				return
			}

			if !errors.Is(errValidate, ErrInvalid) {
				t.Fatalf("Validate error = %v, want ErrInvalid", errValidate)
			}

			if !strings.Contains(errValidate.Error(), tt.wantErr) {
				t.Fatalf("Validate error = %q, want substring %q", errValidate, tt.wantErr)
			}
		})
	}
}

// TestParseDefaults проверяет конфиг по умолчанию и подстановку значений по умолчанию.
func TestParseDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
	}{
		{name: "empty", data: ""},
		{name: "spaces", data: "  \n"},
		{name: "minimal object", data: `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, errParse := Parse([]byte(tt.data))
			if errParse != nil {
				t.Fatalf("Parse: %v", errParse)
			}

			if cfg.Enabled || cfg.ReplyTimeout() != 10*time.Second || cfg.Users == nil || cfg.Commands == nil {
				t.Fatalf("unexpected default config: %+v", cfg)
			}

			if cfg.ConnectTimeout() != 30*time.Second || cfg.PollTimeout() != 30*time.Second {
				t.Fatalf("unexpected default config: %+v", cfg)
			}
		})
	}
}

// TestParseIgnoresUnknownFields проверяет, что лишние поля формы (token_status) не мешают разбору.
func TestParseIgnoresUnknownFields(t *testing.T) {
	t.Parallel()

	cfg, errParse := Parse([]byte(`{"enabled": true, "token_status": "set", "extra": 1}`))
	if errParse != nil {
		t.Fatalf("Parse: %v", errParse)
	}

	if !cfg.Enabled {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

// TestParseRejectsBrokenJSON проверяет ошибку на битом JSON.
func TestParseRejectsBrokenJSON(t *testing.T) {
	t.Parallel()

	_, errParse := Parse([]byte(`{"enabled": tru`))
	if !errors.Is(errParse, ErrInvalid) {
		t.Fatalf("Parse error = %v, want ErrInvalid", errParse)
	}
}

// TestLoadMissingFile проверяет, что отсутствие файла даёт выключенный конфиг.
func TestLoadMissingFile(t *testing.T) {
	t.Parallel()

	cfg, errLoad := Load(filepath.Join(t.TempDir(), "absent.conf"))
	if errLoad != nil {
		t.Fatalf("Load: %v", errLoad)
	}

	if cfg.Enabled {
		t.Fatal("missing config must be disabled")
	}
}

// TestLoadFile проверяет чтение конфига с диска.
func TestLoadFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "wb-telegram.conf")
	data := `{"enabled": true, "users": [{"name": "Я", "chat_id": 1}], "commands": [{"name": "gate", "description": "Ворота"}]}`

	errWrite := os.WriteFile(path, []byte(data), 0o600)
	if errWrite != nil {
		t.Fatalf("WriteFile: %v", errWrite)
	}

	cfg, errLoad := Load(path)
	if errLoad != nil {
		t.Fatalf("Load: %v", errLoad)
	}

	if !cfg.Enabled || cfg.Commands[0].Users == nil {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

// TestLookups проверяет поиск пользователей и команд и права доступа.
func TestLookups(t *testing.T) {
	t.Parallel()

	cfg := validConfig()

	user, ok := cfg.UserByChatID(100)
	if !ok || user.Name != "Супруга" {
		t.Fatalf("UserByChatID(100) = %+v, %v", user, ok)
	}

	if _, ok = cfg.UserByChatID(999); ok {
		t.Fatal("UserByChatID(999) must not be found")
	}

	if _, ok = cfg.UserByName("Ребёнок"); !ok {
		t.Fatal("UserByName must find Ребёнок")
	}

	boiler, _ := cfg.Command("boiler")
	if cfg.Allowed(boiler, "Ребёнок") || !cfg.Allowed(boiler, "Супруга") {
		t.Fatal("boiler must be allowed only to Супруга")
	}

	gate, _ := cfg.Command("gate")
	if !cfg.Allowed(gate, "Ребёнок") {
		t.Fatal("gate with empty users must be allowed to everyone")
	}

	names := func(cmds []Command) []string {
		out := make([]string, 0, len(cmds))

		for _, c := range cmds {
			out = append(out, c.Name)
		}

		return out
	}

	if got := names(cfg.CommandsFor("Ребёнок")); !slices.Equal(got, []string{"gate"}) {
		t.Fatalf("CommandsFor(Ребёнок) = %v", got)
	}

	if got := cfg.NotificationUsers(); len(got) != 1 || got[0].Name != "Супруга" {
		t.Fatalf("NotificationUsers = %+v", got)
	}
}
