package chart

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/chart/mock"
	"github.com/Format-C-eft/wb-telegram/internal/wb_history"
)

// testSpec задаёт условный канал без реальных устройств.
func testSpec() Spec {
	return Spec{Title: "Температура котла", PeriodS: 3600, Channels: []Channel{{Device: "boiler", Control: "temperature", Label: "Подача", Unit: "°C"}}}
}

// TestBuild проверяет PNG и точный диапазон запроса истории.
func TestBuild(t *testing.T) {
	t.Parallel()

	end := time.Date(2026, 11, 10, 14, 0, 0, 0, time.FixedZone("TEST", 5*3600))
	start := end.Add(-time.Hour)
	history := mock.NewMockHistory(gomock.NewController(t))
	history.EXPECT().Read(gomock.Any(), wb_history.Query{Device: "boiler", Control: "temperature", Start: start, End: end}).Return([]wb_history.Point{
		{Time: start.Add(time.Minute), Value: 20, Min: 15, Max: 25, Valid: true, HasMin: true, HasMax: true},
		{Time: end.Add(-time.Minute), Value: 30, Valid: true},
	}, nil)

	builder, errNew := New(history)
	if errNew != nil {
		t.Fatal(errNew)
	}

	result, errBuild := builder.Build(context.Background(), testSpec(), end)
	if errBuild != nil {
		t.Fatal(errBuild)
	}

	decoded, errPNG := png.Decode(bytes.NewReader(result.PNG))
	if errPNG != nil {
		t.Fatal(errPNG)
	}

	if decoded.Bounds().Dx() != 1000 || decoded.Bounds().Dy() != 600 {
		t.Fatal(decoded.Bounds())
	}

	if !strings.Contains(result.Caption, "Температура котла") || !strings.Contains(result.Caption, "1 (синий): Подача [°C]") || !strings.Contains(result.Caption, "+05:00") {
		t.Fatal(result.Caption)
	}

	pixel := decoded.At(left, bottom)

	red, green, blue, alpha := pixel.RGBA()
	if red != uint32(ink.R)*257 || green != uint32(ink.G)*257 || blue != uint32(ink.B)*257 || alpha != 65535 {
		t.Fatalf("axis color=%v", pixel)
	}
}

// TestBuildFailures проверяет отказы до I/O и семантику пустых рядов.
func TestBuildFailures(t *testing.T) {
	t.Parallel()

	end := time.Now()
	tests := []struct {
		name            string
		points          []wb_history.Point
		readError, want error
	}{
		{name: "empty", want: ErrNoData},
		{name: "text", points: []wb_history.Point{{Time: end.Add(-time.Minute)}}, want: ErrNonNumeric},
		{name: "unavailable", readError: wb_history.ErrUnavailable, want: ErrHistory},
		{name: "overflow", readError: wb_history.ErrTooManyData, want: ErrTooLarge},
		{name: "large", points: []wb_history.Point{{Time: end.Add(-time.Minute), Value: math.MaxFloat64, Valid: true}}, want: ErrRange},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			history := mock.NewMockHistory(gomock.NewController(t))
			history.EXPECT().Read(gomock.Any(), gomock.Any()).Return(test.points, test.readError)

			builder, errNew := New(history)
			if errNew != nil {
				t.Fatal(errNew)
			}

			_, errBuild := builder.Build(context.Background(), testSpec(), end)
			if !errors.Is(errBuild, test.want) {
				t.Fatalf("error=%v want=%v", errBuild, test.want)
			}
		})
	}

	history := mock.NewMockHistory(gomock.NewController(t))

	builder, errNew := New(history)
	if errNew != nil {
		t.Fatal(errNew)
	}

	invalid := testSpec()
	invalid.PeriodS = 1

	_, errInvalid := builder.Build(context.Background(), invalid, end)
	if !errors.Is(errInvalid, ErrInvalidArgument) {
		t.Fatal(errInvalid)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, errCanceled := builder.Build(ctx, testSpec(), end)
	if !errors.Is(errCanceled, ErrCanceled) {
		t.Fatal(errCanceled)
	}
}

// TestValidate проверяет MQTT-имена, повтор каналов и единицы.
func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(*Spec)
	}{
		{"period", func(spec *Spec) { spec.PeriodS = 7*24*3600 + 1 }},
		{"title", func(spec *Spec) { spec.Title = strings.Repeat("😀", 41) }},
		{"wildcard", func(spec *Spec) { spec.Channels[0].Control = "a/+" }},
		{"duplicate", func(spec *Spec) { spec.Channels = append(spec.Channels, spec.Channels[0]) }},
		{"unit", func(spec *Spec) {
			spec.Channels = append(spec.Channels, Channel{Device: "other", Control: "value", Label: "Давление", Unit: "bar"})
		}},
		{"control", func(spec *Spec) { spec.Title = "x\ny" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			spec := testSpec()
			test.change(&spec)

			if !errors.Is(spec.Validate(), ErrInvalidArgument) {
				t.Fatal("accepted invalid spec")
			}
		})
	}
}

// TestSamplePNG сохраняет доступный для просмотра образец при явном пути.
func TestSamplePNG(t *testing.T) {
	t.Parallel()

	enabled := os.Getenv("CHART_SAMPLE_PATH")
	if enabled == "" {
		t.Skip("sample path not set")
	}

	end := time.Date(2026, 11, 10, 16, 0, 0, 0, time.FixedZone("TEST", 5*3600))
	start := end.Add(-24 * time.Hour)
	series := make([][]wb_history.Point, 4)

	for index := range series {
		for sample := range 100 {
			point := wb_history.Point{Time: start.Add(time.Duration(sample+1) * time.Hour / 5), Value: 20 + float64(index)*5 + math.Sin(float64(sample)/8)*3, Valid: true}
			if index == 0 {
				point.HasMin = true
				point.HasMax = true
				point.Min = point.Value - 2
				point.Max = point.Value + 2
			}

			if sample > 40 && sample < 50 && index == 1 {
				point.Valid = false
			}

			series[index] = append(series[index], point)
		}
	}

	payload, errRender := render(context.Background(), series, start, end)
	if errRender != nil {
		t.Fatal(errRender)
	}

	errWrite := os.WriteFile("../../tmp/chart-sample.png", payload, 0o600)
	if errWrite != nil {
		t.Fatal(errWrite)
	}
}

// TestPartialSeries проверяет частично пустой график и общий бюджет чтения.
func TestPartialSeries(t *testing.T) {
	t.Parallel()

	end := time.Date(2026, 3, 29, 3, 30, 0, 0, time.FixedZone("TEST", 2*3600))
	spec := testSpec()
	spec.PeriodS = 7 * 24 * 3600
	spec.Channels = append(spec.Channels, Channel{Device: "second", Control: "temperature", Label: "Обратка", Unit: "°C"})
	history := mock.NewMockHistory(gomock.NewController(t))

	var firstDeadline time.Time

	gomock.InOrder(
		history.EXPECT().Read(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, _ wb_history.Query) ([]wb_history.Point, error) {
			firstDeadline, _ = ctx.Deadline()

			return nil, nil
		}),
		history.EXPECT().Read(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, query wb_history.Query) ([]wb_history.Point, error) {
			deadline, ok := ctx.Deadline()
			if !ok || !deadline.Equal(firstDeadline) || query.End != end || !query.Start.Equal(end.Add(-7*24*time.Hour)) {
				t.Error("history budget or range changed")
			}

			return []wb_history.Point{{Time: end.Add(-time.Hour), Value: 4, Valid: true}}, nil
		}),
	)

	builder, errNew := New(history)
	if errNew != nil {
		t.Fatal(errNew)
	}

	result, errBuild := builder.Build(context.Background(), spec, end)
	if errBuild != nil {
		t.Fatal(errBuild)
	}

	if !strings.Contains(result.Caption, "1 (синий): Подача [°C] - нет числовых данных") || !strings.Contains(result.Caption, "2 (красный): Обратка") {
		t.Fatal(result.Caption)
	}
}
