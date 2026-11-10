package chart

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"testing"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/wb_history"
)

// TestRenderPoints проверяет одиночную точку, диапазон агрегата и явный разрыв.
func TestRenderPoints(t *testing.T) {
	t.Parallel()

	start := time.Unix(0, 0)
	end := start.Add(time.Hour)
	tests := []struct {
		name   string
		points []wb_history.Point
	}{
		{"one", []wb_history.Point{{Time: start.Add(time.Minute), Value: 2, Valid: true}}},
		{"negative", []wb_history.Point{{Time: start.Add(time.Minute), Value: -2, Min: -10, Max: -1, HasMin: true, HasMax: true, Valid: true}, {Time: end.Add(-time.Minute), Value: -3, Valid: true}}},
		{"gap", []wb_history.Point{{Time: start.Add(time.Minute), Value: 2, Valid: true}, {Time: start.Add(time.Hour / 2), Valid: false}, {Time: end.Add(-time.Minute), Value: 3, Valid: true}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			data, errRender := render(context.Background(), [][]wb_history.Point{test.points}, start, end)
			if errRender != nil {
				t.Fatal(errRender)
			}

			canvas, errPNG := png.Decode(bytes.NewReader(data))
			if errPNG != nil {
				t.Fatal(errPNG)
			}

			low, high := bounds([][]wb_history.Point{test.points})
			point := test.points[0]
			x := coordinate(float64(point.Time.Sub(start))/float64(end.Sub(start)), left, right)
			y := coordinate((high-point.Value)/(high-low), top, bottom)

			red, green, blue, _ := canvas.At(x, y).RGBA()
			if red != uint32(palette[0].R)*257 || green != uint32(palette[0].G)*257 || blue != uint32(palette[0].B)*257 {
				t.Fatalf("marker absent at %d,%d", x, y)
			}

			if test.name == "gap" {
				red, green, blue, _ = canvas.At((left+right)/2, (top+bottom)/2).RGBA()
				if red == uint32(palette[0].R)*257 && green == uint32(palette[0].G)*257 && blue == uint32(palette[0].B)*257 {
					t.Fatal("line crossed gap")
				}
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, errCanceled := render(ctx, nil, start, end)
	if !errors.Is(errCanceled, ErrCanceled) {
		t.Fatal(errCanceled)
	}
}
