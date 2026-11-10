package bot

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

const (
	panelTTL      = 5 * time.Minute
	maxPanels     = 1024
	maxUserPanels = 32
)

// buttonSpec описывает действие кнопки от правила.
type buttonSpec struct {
	Text    string `json:"text"`
	Command string `json:"command"`
	Args    string `json:"args"`
	Cancel  bool   `json:"cancel"`
}

// buttonPanel хранит одноразовую клавиатуру получателя.
type buttonPanel struct {
	id       string
	chatID   int64
	expires  time.Time
	keyboard telegram.InlineKeyboard
	tokens   []string
}

// buttonAction связывает непрозрачный токен с панелью и действием.
type buttonAction struct {
	panel *buttonPanel
	spec  buttonSpec
}

// validateButtons проверяет размеры клавиатуры и сочетания действий.
func validateButtons(rows [][]buttonSpec) error {
	if len(rows) == 0 || len(rows) > 8 {
		return badPayload("invalid keyboard rows")
	}

	for _, row := range rows {
		if len(row) == 0 || len(row) > 4 {
			return badPayload("invalid keyboard columns")
		}

		for _, button := range row {
			errButton := button.validate()
			if errButton != nil {
				return errButton
			}
		}
	}

	return nil
}

// validate проверяет подпись и действие кнопки.
func (button buttonSpec) validate() error {
	if strings.TrimSpace(button.Text) == "" || len(utf16.Encode([]rune(button.Text))) > 64 || !utf8.ValidString(button.Args) || len(button.Args) > 256 {
		return badPayload("invalid button label or arguments")
	}

	if button.Cancel {
		if button.Command != "" || button.Args != "" {
			return badPayload("cancel button cannot invoke a command")
		}

		return nil
	}

	if button.Command == "" || strings.HasPrefix(button.Command, "/") {
		return badPayload("button command is required without slash")
	}

	return nil
}

// makeButtonToken создаёт непредсказуемый токен callback.
func makeButtonToken() (string, error) {
	data := make([]byte, 16)

	_, errRead := rand.Read(data)
	if errRead != nil {
		return "", &Error{kind: kindInvalidState, Op: "button token", Message: "random source failed"}
	}

	return "b1:" + base64.RawURLEncoding.EncodeToString(data), nil
}

// preparePanel регистрирует отдельную панель для одного адресата.
func (b *Bot) preparePanel(chatID int64, rows [][]buttonSpec) (*buttonPanel, error) {
	user, known := b.cfg.UserByChatID(chatID)
	if !known || chatID <= 0 {
		return nil, badPayload("buttons require a configured private recipient")
	}

	panel, actions, errBuild := b.buildPanel(chatID, user.Name, rows)
	if errBuild != nil {
		return nil, errBuild
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stopped {
		return nil, ErrInvalidState
	}

	b.prunePanelsLocked(b.options.now())

	count := 0

	for _, active := range b.panels {
		if active.chatID == chatID {
			count++
		}
	}

	if len(b.panels) >= maxPanels || count >= maxUserPanels {
		return nil, badPayload("too many active keyboards")
	}

	for token := range actions {
		if _, exists := b.buttons[token]; exists {
			return nil, badPayload("duplicate callback token")
		}
	}

	b.panels[panel.id] = panel
	for token, action := range actions {
		b.buttons[token] = buttonAction{panel: panel, spec: action}
	}

	return panel, nil
}

// buildPanel готовит клавиатуру вне блокировки состояния.
func (b *Bot) buildPanel(chatID int64, userName string, rows [][]buttonSpec) (*buttonPanel, map[string]buttonSpec, error) {
	panel := &buttonPanel{chatID: chatID, expires: b.options.now().Add(panelTTL)}

	actions := make(map[string]buttonSpec)
	for _, row := range rows {
		buttons := make([]telegram.InlineButton, 0, len(row))
		for _, button := range row {
			if !button.Cancel {
				command, found := b.cfg.Command(button.Command)
				if !found || !b.cfg.Allowed(command, userName) {
					return nil, nil, badPayload("button command is unavailable")
				}
			}

			token, errToken := b.options.buttonToken()
			if errToken != nil {
				return nil, nil, errToken
			}

			if _, exists := actions[token]; exists {
				return nil, nil, badPayload("duplicate callback token")
			}

			actions[token] = button
			panel.tokens = append(panel.tokens, token)
			buttons = append(buttons, telegram.InlineButton{Text: button.Text, Data: token})
		}

		panel.keyboard = append(panel.keyboard, buttons)
	}

	panel.id = panel.tokens[0]

	return panel, actions, nil
}

// prunePanelsLocked удаляет истёкшие панели под mutex ядра.
func (b *Bot) prunePanelsLocked(now time.Time) {
	for _, panel := range b.panels {
		if !now.Before(panel.expires) {
			b.removePanelLocked(panel)
		}
	}
}

// removePanelLocked удаляет все токены панели под mutex ядра.
func (b *Bot) removePanelLocked(panel *buttonPanel) {
	delete(b.panels, panel.id)

	for _, token := range panel.tokens {
		delete(b.buttons, token)
	}
}

// sendInteraction ставит форматированный текст и клавиатуру в очередь.
func (b *Bot) sendInteraction(chatID int64, req sendRequest) {
	delivery := telegram.Delivery{Kind: telegram.DeliveryText, Text: req.Text, ParseMode: req.ParseMode}

	var (
		panel    *buttonPanel
		errPanel error
	)
	if req.Buttons != nil {
		panel, errPanel = b.preparePanel(chatID, req.Buttons)
		if errPanel != nil {
			b.reportInteractionError(chatID, errPanel)

			return
		}

		delivery.Keyboard = panel.keyboard
	}

	errSend := b.sender.SendDelivery(chatID, delivery)
	if errSend == nil || errors.Is(errSend, telegram.ErrQueueFull) {
		return
	}

	if panel != nil {
		b.mu.Lock()
		b.removePanelLocked(panel)
		b.mu.Unlock()
	}

	b.reportInteractionError(chatID, errSend)
}
