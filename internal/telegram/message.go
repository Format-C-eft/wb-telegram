package telegram

import "unicode/utf16"

// DeliveryKind определяет вид сообщения.
type DeliveryKind string

const (
	DeliveryText  DeliveryKind = "text"
	DeliveryPhoto DeliveryKind = "photo"
)

// ParseMode определяет разметку текста.
type ParseMode string

const (
	ParseModePlain      ParseMode = ""
	ParseModeHTML       ParseMode = "HTML"
	ParseModeMarkdownV2 ParseMode = "MarkdownV2"
	MaxPNGBytes                   = 2 << 20
	maxQueuePNGBytes              = 16 << 20
)

// InlineButton содержит подпись и непрозрачные данные кнопки.
type InlineButton struct{ Text, Data string }

// InlineKeyboard содержит строки кнопок.
type InlineKeyboard = [][]InlineButton

// Delivery содержит самостоятельное исходящее сообщение.
type Delivery struct {
	Kind      DeliveryKind
	Text      string
	Caption   string
	ParseMode ParseMode
	Keyboard  InlineKeyboard
	PNG       []byte
}

// utf16Length возвращает длину строки в единицах UTF-16.
func utf16Length(text string) int {
	count := 0

	for _, char := range text {
		count += utf16.RuneLen(char)
	}

	return count
}

// validate проверяет сочетания и пределы доставки.
func (d Delivery) validate() error {
	if d.ParseMode != ParseModePlain && d.ParseMode != ParseModeHTML && d.ParseMode != ParseModeMarkdownV2 {
		return ErrInvalidArgument
	}

	errContent := d.validateContent()
	if errContent != nil {
		return errContent
	}

	for _, row := range d.Keyboard {
		for _, button := range row {
			if button.Text == "" || utf16Length(button.Text) > 64 || len(button.Data) < 1 || len(button.Data) > 64 {
				return ErrInvalidArgument
			}
		}
	}

	return nil
}

// clone копирует изменяемые данные доставки.
func (d Delivery) clone() Delivery {
	d.PNG = append([]byte(nil), d.PNG...)
	keyboard := make(InlineKeyboard, len(d.Keyboard))

	for index, row := range d.Keyboard {
		keyboard[index] = append([]InlineButton(nil), row...)
	}

	d.Keyboard = keyboard

	return d
}

// validateContent проверяет поля выбранного вида доставки.
func (d Delivery) validateContent() error {
	switch d.Kind {
	case DeliveryText:
		if d.Text == "" || d.Caption != "" || len(d.PNG) != 0 {
			return ErrInvalidArgument
		}

		if d.ParseMode != ParseModePlain && utf16Length(d.Text) > MaxMessageRunes {
			return ErrInvalidArgument
		}
	case DeliveryPhoto:
		if d.Text != "" || len(d.PNG) == 0 || len(d.PNG) > MaxPNGBytes || len(d.Keyboard) != 0 || utf16Length(d.Caption) > 1024 {
			return ErrInvalidArgument
		}
	default:
		return ErrInvalidArgument
	}

	return nil
}
