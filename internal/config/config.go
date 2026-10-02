package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultConfigPath — путь конфига, который пишет wb-mqtt-confed.
	DefaultConfigPath = "/etc/wb-telegram.conf"
	// DefaultTokenPath — путь файла с токеном бота.
	DefaultTokenPath = "/etc/wb-telegram/token" //nolint:gosec // это путь к файлу, а не секрет
	// DefaultReplyTimeoutS — таймаут ответа правила по умолчанию, с.
	DefaultReplyTimeoutS = 10
	// MaxReplyTimeoutS — максимальный таймаут ответа правила, с.
	MaxReplyTimeoutS = 300
	// DefaultConnectTimeoutS — таймаут подключения к Telegram (TCP и TLS) по умолчанию, с.
	DefaultConnectTimeoutS = 30
	// MinConnectTimeoutS — минимальный таймаут подключения к Telegram, с.
	MinConnectTimeoutS = 5
	// MaxConnectTimeoutS — максимальный таймаут подключения к Telegram, с.
	MaxConnectTimeoutS = 120
	// DefaultPollTimeoutS — таймаут long polling getUpdates по умолчанию, с.
	DefaultPollTimeoutS = 30
	// MaxPollTimeoutS — максимальный таймаут long polling, с (ограничение Telegram — 50).
	MaxPollTimeoutS = 50
	// MaxCommands — лимит команд Telegram.
	MaxCommands = 100
	// maxDescriptionRunes — лимит длины описания команды Telegram.
	maxDescriptionRunes = 256
)

// commandNamePattern — допустимое имя команды Telegram.
var commandNamePattern = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// reservedCommands — команды, которые обрабатывает сам бот.
var reservedCommands = []string{"start", "help"}

// User — член семьи, которому разрешено пользоваться ботом.
type User struct {
	Name          string `json:"name"`
	ChatID        int64  `json:"chat_id"`
	Notifications bool   `json:"notifications"`
}

// Command — команда бота; пустой Users означает «доступна всем».
type Command struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Users       []string `json:"users"`
}

// Config — конфигурация сервиса в том виде, в котором она лежит на диске.
type Config struct {
	Enabled         bool      `json:"enabled"`
	ConnectTimeoutS int       `json:"connect_timeout_s"`
	PollTimeoutS    int       `json:"poll_timeout_s"`
	ReplyTimeoutS   int       `json:"reply_timeout_s"`
	Debug           bool      `json:"debug"`
	Users           []User    `json:"users"`
	Commands        []Command `json:"commands"`
}

// Load читает конфиг с диска; отсутствующий файл даёт выключенный конфиг по умолчанию.
func Load(path string) (*Config, error) {
	data, errRead := os.ReadFile(path) //nolint:gosec // путь задаёт администратор через флаг запуска
	if errors.Is(errRead, fs.ErrNotExist) {
		return Parse(nil)
	}

	if errRead != nil {
		return nil, &Error{kind: kindIO, Op: "read", Path: path, Message: errRead.Error()}
	}

	return Parse(data)
}

// Parse разбирает и проверяет конфиг; пустой ввод даёт выключенный конфиг по умолчанию.
// Неизвестные поля JSON (например, token_status из формы) игнорируются.
func Parse(data []byte) (*Config, error) {
	cfg, errDecode := decode(data)
	if errDecode != nil {
		return nil, errDecode
	}

	errValidate := cfg.Validate()
	if errValidate != nil {
		return nil, errValidate
	}

	return cfg, nil
}

// decode разбирает JSON без проверки правил и подставляет значения по умолчанию.
func decode(data []byte) (*Config, error) {
	cfg := &Config{}

	if len(bytes.TrimSpace(data)) > 0 {
		errUnmarshal := json.Unmarshal(data, cfg)
		if errUnmarshal != nil {
			return nil, &Error{kind: kindInvalid, Op: "decode", Message: errUnmarshal.Error()}
		}
	}

	cfg.applyDefaults()

	return cfg, nil
}

// applyDefaults подставляет значения по умолчанию и пустые срезы вместо nil.
func (c *Config) applyDefaults() {
	if c.ReplyTimeoutS == 0 {
		c.ReplyTimeoutS = DefaultReplyTimeoutS
	}

	if c.ConnectTimeoutS == 0 {
		c.ConnectTimeoutS = DefaultConnectTimeoutS
	}

	if c.PollTimeoutS == 0 {
		c.PollTimeoutS = DefaultPollTimeoutS
	}

	if c.Users == nil {
		c.Users = []User{}
	}

	if c.Commands == nil {
		c.Commands = []Command{}
	}

	for i := range c.Commands {
		if c.Commands[i].Users == nil {
			c.Commands[i].Users = []string{}
		}
	}
}

// Validate проверяет правила, которые нельзя выразить JSON Schema, и дублирует проверки схемы.
func (c *Config) Validate() error {
	var problems []string

	if c.ReplyTimeoutS < 1 || c.ReplyTimeoutS > MaxReplyTimeoutS {
		problems = append(problems, fmt.Sprintf("reply_timeout_s должен быть от 1 до %d", MaxReplyTimeoutS))
	}

	if c.ConnectTimeoutS < MinConnectTimeoutS || c.ConnectTimeoutS > MaxConnectTimeoutS {
		problems = append(problems, fmt.Sprintf("connect_timeout_s должен быть от %d до %d", MinConnectTimeoutS, MaxConnectTimeoutS))
	}

	if c.PollTimeoutS < 1 || c.PollTimeoutS > MaxPollTimeoutS {
		problems = append(problems, fmt.Sprintf("poll_timeout_s должен быть от 1 до %d", MaxPollTimeoutS))
	}

	problems = append(problems, c.validateUsers()...)
	problems = append(problems, c.validateCommands()...)

	if len(problems) > 0 {
		return &Error{kind: kindInvalid, Op: "validate", Message: strings.Join(problems, "; ")}
	}

	return nil
}

// validateUsers проверяет список пользователей.
func (c *Config) validateUsers() []string {
	var problems []string

	names := map[string]bool{}
	chats := map[int64]bool{}

	for i, user := range c.Users {
		if strings.TrimSpace(user.Name) == "" {
			problems = append(problems, fmt.Sprintf("пользователь %d: пустое имя пользователя", i+1))
		}

		if names[user.Name] {
			problems = append(problems, fmt.Sprintf("повторяется имя пользователя %q", user.Name))
		}

		if user.ChatID == 0 {
			problems = append(problems, fmt.Sprintf("пользователь %q: не задан chat_id", user.Name))
		}

		if chats[user.ChatID] && user.ChatID != 0 {
			problems = append(problems, fmt.Sprintf("повторяется chat_id %d", user.ChatID))
		}

		names[user.Name] = true
		chats[user.ChatID] = true
	}

	return problems
}

// validateCommands проверяет список команд.
func (c *Config) validateCommands() []string {
	var problems []string

	if len(c.Commands) > MaxCommands {
		problems = append(problems, fmt.Sprintf("команд должно быть не больше %d", MaxCommands))
	}

	seen := map[string]bool{}

	for _, cmd := range c.Commands {
		if !commandNamePattern.MatchString(cmd.Name) {
			problems = append(problems, fmt.Sprintf("имя команды %q: только a-z, 0-9, _ и не длиннее 32 символов", cmd.Name))
		}

		if slices.Contains(reservedCommands, cmd.Name) {
			problems = append(problems, fmt.Sprintf("команда /%s зарезервирована ботом", cmd.Name))
		}

		if seen[cmd.Name] {
			problems = append(problems, fmt.Sprintf("повторяется команда /%s", cmd.Name))
		}

		seen[cmd.Name] = true

		descriptionLen := utf8.RuneCountInString(strings.TrimSpace(cmd.Description))
		if descriptionLen < 1 || descriptionLen > maxDescriptionRunes {
			problems = append(problems, fmt.Sprintf("команда /%s: описание должно быть от 1 до %d символов", cmd.Name, maxDescriptionRunes))
		}

		problems = append(problems, c.validateCommandUsers(cmd)...)
	}

	return problems
}

// validateCommandUsers проверяет список пользователей команды.
func (c *Config) validateCommandUsers(cmd Command) []string {
	var problems []string

	seen := map[string]bool{}

	for _, name := range cmd.Users {
		if _, ok := c.UserByName(name); !ok {
			problems = append(problems, fmt.Sprintf("команда /%s: неизвестный пользователь %q", cmd.Name, name))
		}

		if seen[name] {
			problems = append(problems, fmt.Sprintf("команда /%s: повторяется пользователь %q", cmd.Name, name))
		}

		seen[name] = true
	}

	return problems
}

// ConnectTimeout возвращает таймаут подключения к Telegram (TCP и TLS).
func (c *Config) ConnectTimeout() time.Duration {
	return time.Duration(c.ConnectTimeoutS) * time.Second
}

// PollTimeout возвращает таймаут long polling getUpdates.
func (c *Config) PollTimeout() time.Duration {
	return time.Duration(c.PollTimeoutS) * time.Second
}

// ReplyTimeout возвращает таймаут ответа правила.
func (c *Config) ReplyTimeout() time.Duration {
	return time.Duration(c.ReplyTimeoutS) * time.Second
}

// UserByChatID ищет пользователя по chat_id.
func (c *Config) UserByChatID(chatID int64) (User, bool) {
	for _, user := range c.Users {
		if user.ChatID == chatID {
			return user, true
		}
	}

	return User{}, false
}

// UserByName ищет пользователя по имени.
func (c *Config) UserByName(name string) (User, bool) {
	for _, user := range c.Users {
		if user.Name == name {
			return user, true
		}
	}

	return User{}, false
}

// Command ищет команду по имени.
func (c *Config) Command(name string) (Command, bool) {
	for _, cmd := range c.Commands {
		if cmd.Name == name {
			return cmd, true
		}
	}

	return Command{}, false
}

// Allowed сообщает, доступна ли команда пользователю.
func (c *Config) Allowed(cmd Command, userName string) bool {
	return len(cmd.Users) == 0 || slices.Contains(cmd.Users, userName)
}

// CommandsFor возвращает команды, доступные пользователю, в порядке настроек.
func (c *Config) CommandsFor(userName string) []Command {
	var out []Command

	for _, cmd := range c.Commands {
		if c.Allowed(cmd, userName) {
			out = append(out, cmd)
		}
	}

	return out
}

// NotificationUsers возвращает пользователей с включёнными системными уведомлениями.
func (c *Config) NotificationUsers() []User {
	var out []User

	for _, user := range c.Users {
		if user.Notifications {
			out = append(out, user)
		}
	}

	return out
}
