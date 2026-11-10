// Package chart строит ограниченные PNG-графики числовой истории WB.
package chart

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Format-C-eft/wb-telegram/internal/wb_history"
)

type kind uint8

const (
	kindInvalid kind = iota + 1
	kindHistory
	kindNoData
	kindNonNumeric
	kindRange
	kindCanceled
	kindTooLarge
)

// Error представляет отказ построения графика.
type Error struct{ kind kind }

// Error возвращает безопасный текст ошибки.
func (e *Error) Error() string {
	switch e.kind {
	case kindInvalid:
		return "некорректные параметры графика"
	case kindNoData:
		return "За выбранный период данных нет"
	case kindNonNumeric:
		return "Канал не содержит числовых данных"
	case kindRange:
		return "значения графика вне допустимого диапазона"
	case kindCanceled:
		return "построение графика отменено"
	case kindTooLarge:
		return "график превышает допустимый размер"
	default:
		return "не удалось получить историю"
	}
}

// Is сравнивает вид ошибки.
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)

	return ok && e.kind == other.kind
}

var (
	ErrInvalidArgument = &Error{kind: kindInvalid}
	ErrHistory         = &Error{kind: kindHistory}
	ErrNoData          = &Error{kind: kindNoData}
	ErrNonNumeric      = &Error{kind: kindNonNumeric}
	ErrRange           = &Error{kind: kindRange}
	ErrCanceled        = &Error{kind: kindCanceled}
	ErrTooLarge        = &Error{kind: kindTooLarge}
)

// Spec задаёт график общего периода и единицы измерения.
type Spec struct {
	Title    string    `json:"title"`
	PeriodS  int64     `json:"period_s"`
	Channels []Channel `json:"channels"`
}

// Channel задаёт канал и подпись ряда.
type Channel struct {
	Device  string `json:"device"`
	Control string `json:"control"`
	Label   string `json:"label"`
	Unit    string `json:"unit"`
}

// Result содержит готовое изображение и plain-text подпись.
type Result struct {
	PNG     []byte
	Caption string
}

// History предоставляет историю одного канала.
type History interface {
	Read(context.Context, wb_history.Query) ([]wb_history.Point, error)
}

// Builder строит графики через источник истории.
type Builder struct {
	history History
	options options
}

// New создаёт построитель графиков.
func New(history History, opts ...Option) (*Builder, error) {
	o := options{historyBudget: 10 * time.Second}

	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	if history == nil {
		return nil, ErrInvalidArgument
	}

	errValidate := o.Validate()
	if errValidate != nil {
		return nil, errValidate
	}

	return &Builder{history: history, options: o}, nil
}

// Validate проверяет параметры до чтения истории.
func (spec Spec) Validate() error {
	if !validText(spec.Title, 80, false) || spec.PeriodS < 60 || spec.PeriodS > 7*24*60*60 || len(spec.Channels) < 1 || len(spec.Channels) > 4 {
		return ErrInvalidArgument
	}

	seen := map[[2]string]bool{}
	unit := spec.Channels[0].Unit

	for _, channel := range spec.Channels {
		key := [2]string{channel.Device, channel.Control}
		if !wb_history.ValidIdentifier(channel.Device) || !wb_history.ValidIdentifier(channel.Control) || !validText(channel.Label, 80, false) || !validText(channel.Unit, 16, true) || channel.Unit != unit || seen[key] {
			return ErrInvalidArgument
		}

		seen[key] = true
	}

	return nil
}

// validText проверяет короткую plain-text подпись.
func validText(value string, limit int, allowEmpty bool) bool {
	if !utf8.ValidString(value) || (!allowEmpty && strings.TrimSpace(value) == "") || len(utf16.Encode([]rune(value))) > limit {
		return false
	}

	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}

	return true
}

// Build запрашивает историю и рисует график фиксированного интервала.
func (b *Builder) Build(ctx context.Context, spec Spec, end time.Time) (Result, error) {
	errValidate := spec.Validate()
	if errValidate != nil {
		return Result{}, errValidate
	}

	start := end.Add(-time.Duration(spec.PeriodS) * time.Second)

	_, errCaption := makeCaption(spec, start, end, nil)
	if errCaption != nil {
		return Result{}, errCaption
	}

	historyContext, cancel := context.WithTimeout(ctx, b.options.historyBudget)
	defer cancel()

	series := make([][]wb_history.Point, len(spec.Channels))
	anyValid := false
	anyRecords := false
	missing := make([]bool, len(series))

	for index, channel := range spec.Channels {
		if historyContext.Err() != nil {
			return Result{}, ErrCanceled
		}

		points, errRead := b.history.Read(historyContext, wb_history.Query{Device: channel.Device, Control: channel.Control, Start: start, End: end})
		if errRead != nil {
			return Result{}, historyError(historyContext, errRead)
		}

		normalized, errNormalize := normalize(points, start, end)
		if errNormalize != nil {
			return Result{}, errNormalize
		}

		series[index] = normalized
		anyRecords = anyRecords || len(normalized) > 0
		missing[index] = true

		for _, point := range normalized {
			if point.Valid {
				anyValid = true
				missing[index] = false
			}
		}
	}

	if !anyValid {
		if anyRecords {
			return Result{}, ErrNonNumeric
		}

		return Result{}, ErrNoData
	}

	var caption string

	caption, errCaption = makeCaption(spec, start, end, missing)
	if errCaption != nil {
		return Result{}, errCaption
	}

	png, errRender := render(ctx, series, start, end)
	if errRender != nil {
		return Result{}, errRender
	}

	return Result{PNG: png, Caption: caption}, nil
}

// makeCaption связывает номера и цвета с именами рядов.
func makeCaption(spec Spec, start, end time.Time, missing []bool) (string, error) {
	var caption strings.Builder
	caption.WriteString(spec.Title + "\n" + start.Format("02.01.2006 15:04:05 MST -07:00") + " - " + end.Format("02.01.2006 15:04:05 MST -07:00") + "\nИнтервал открытый; данные агрегированы; линии соединяют записанные значения.")

	colors := []string{"синий", "красный", "зелёный", "фиолетовый"}

	for index, channel := range spec.Channels {
		fmt.Fprintf(&caption, "\n%d (%s): %s", index+1, colors[index], channel.Label)

		if channel.Unit != "" {
			caption.WriteString(" [" + channel.Unit + "]")
		}

		if len(missing) > index && missing[index] {
			caption.WriteString(" - нет числовых данных")
		}
	}

	if len(utf16.Encode([]rune(caption.String()))) > 1024 {
		return "", ErrInvalidArgument
	}

	return caption.String(), nil
}

// historyError переводит отказ источника в ошибку графика.
func historyError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ErrCanceled
	}

	if errors.Is(err, wb_history.ErrTooManyData) {
		return ErrTooLarge
	}

	return ErrHistory
}
