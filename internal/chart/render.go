package chart

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strconv"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/wb_history"
)

const (
	width  = 1000
	height = 600
	left   = 140
	right  = 965
	top    = 36
	bottom = 530
)

var palette = []color.RGBA{{30, 90, 200, 255}, {210, 55, 45, 255}, {25, 135, 75, 255}, {140, 65, 185, 255}}
var ink = color.RGBA{45, 50, 60, 255}

// render создаёт непрозрачный PNG с общей шкалой рядов.
func render(ctx context.Context, series [][]wb_history.Point, start, end time.Time) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ErrCanceled
	}

	minimum, maximum := bounds(series)
	if !finite(minimum) || !finite(maximum) || minimum >= maximum {
		return nil, ErrRange
	}

	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.RGBA{255, 255, 255, 255}), image.Point{}, draw.Src)

	grid := color.RGBA{222, 227, 234, 255}

	for tick := range 6 {
		y := bottom - tick*(bottom-top)/5
		line(canvas, left, y, right, y, grid)

		value := minimum + float64(tick)*(maximum-minimum)/5
		label := strconv.FormatFloat(value, 'g', 4, 64)
		text(canvas, left-14-len(label)*12, y-7, label, ink, 2)
	}

	line(canvas, left, top, left, bottom, ink)
	line(canvas, left, bottom, right, bottom, ink)

	for tick := range 5 {
		x := left + tick*(right-left)/4
		instant := start.Add(time.Duration(float64(end.Sub(start)) * float64(tick) / 4))

		label := instant.Format("15:04")
		if end.Sub(start) > 24*time.Hour {
			label = instant.Format("02.01 15:04")
		}

		labelX := max(8, min(x-len(label)*6, width-8-len(label)*12))
		text(canvas, labelX, bottom+12, label, ink, 2)
	}

	_, offset := end.Zone()

	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}

	text(canvas, left, bottom+42, sign+twoDigits(offset/3600)+":"+twoDigits(offset%3600/60), ink, 2)

	for index, points := range series {
		errSeries := renderSeries(ctx, canvas, points, index, start, end, minimum, maximum)
		if errSeries != nil {
			return nil, errSeries
		}
	}

	if ctx.Err() != nil {
		return nil, ErrCanceled
	}

	var output bytes.Buffer

	encoder := png.Encoder{CompressionLevel: png.BestSpeed}

	errEncode := encoder.Encode(&output, canvas)
	if errEncode != nil {
		return nil, ErrTooLarge
	}

	if ctx.Err() != nil {
		return nil, ErrCanceled
	}

	if output.Len() > 2*1024*1024 {
		return nil, ErrTooLarge
	}

	return output.Bytes(), nil
}

// bounds определяет шкалу с учётом агрегатных экстремумов.
func bounds(series [][]wb_history.Point) (float64, float64) {
	minimum, maximum := math.Inf(1), math.Inf(-1)

	for _, points := range series {
		for _, point := range points {
			if !point.Valid {
				continue
			}

			minimum = min(minimum, point.Value)

			maximum = max(maximum, point.Value)
			if point.HasMin {
				minimum = min(minimum, point.Min)
			}

			if point.HasMax {
				maximum = max(maximum, point.Max)
			}
		}
	}

	if !finite(minimum) || !finite(maximum) {
		return minimum, maximum
	}

	span := maximum - minimum
	threshold := max(math.Abs(minimum), math.Abs(maximum), 1) * 1e-9

	padding := span * 0.05
	if span < threshold {
		padding = max(math.Abs(minimum)*0.05, 1)
	}

	return minimum - padding, maximum + padding
}

// coordinate переводит безопасную долю в ограниченную координату.
func coordinate(fraction float64, low, high int) int {
	if !finite(fraction) {
		return low
	}

	return low + int(math.Round(max(0, min(fraction, 1))*float64(high-low)))
}

// line рисует отрезок алгоритмом Брезенхема.
func line(canvas *image.RGBA, x0, y0, x1, y1 int, shade color.RGBA) {
	dx := abs(x1 - x0)
	dy := -abs(y1 - y0)

	sx, sy := -1, -1
	if x0 < x1 {
		sx = 1
	}

	if y0 < y1 {
		sy = 1
	}

	deviation := dx + dy

	for {
		canvas.SetRGBA(x0, y0, shade)

		if x0 == x1 && y0 == y1 {
			return
		}

		twice := 2 * deviation
		if twice >= dy {
			deviation += dy
			x0 += sx
		}

		if twice <= dx {
			deviation += dx
			y0 += sy
		}
	}
}

// abs возвращает модуль целой координаты.
func abs(value int) int {
	if value < 0 {
		return -value
	}

	return value
}

// twoDigits форматирует компонент смещения часового пояса.
func twoDigits(value int) string {
	if value < 10 {
		return "0" + strconv.Itoa(value)
	}

	return strconv.Itoa(value)
}

// renderSeries рисует линию и диапазоны одного ряда.
func renderSeries(ctx context.Context, canvas *image.RGBA, points []wb_history.Point, index int, start, end time.Time, minimum, maximum float64) error {
	previous := image.Point{}
	connected := false
	shade := palette[index]
	text(canvas, right-95+index*23, 10, strconv.Itoa(index+1), shade, 2)

	for _, point := range points {
		if ctx.Err() != nil {
			return ErrCanceled
		}

		if !point.Valid {
			connected = false

			continue
		}

		x := coordinate(float64(point.Time.Sub(start))/float64(end.Sub(start)), left, right)

		y := coordinate((maximum-point.Value)/(maximum-minimum), top, bottom)
		if point.HasMin || point.HasMax {
			low, high := point.Value, point.Value
			if point.HasMin {
				low = point.Min
			}

			if point.HasMax {
				high = point.Max
			}

			minY := coordinate((maximum-high)/(maximum-minimum), top, bottom)
			maxY := coordinate((maximum-low)/(maximum-minimum), top, bottom)
			line(canvas, x, minY, x, maxY, shade)
		}

		if connected {
			line(canvas, previous.X, previous.Y, x, y, shade)
		}

		draw.Draw(canvas, image.Rect(x-2, y-2, x+3, y+3), image.NewUniform(shade), image.Point{}, draw.Src)
		previous = image.Pt(x, y)
		connected = true
	}

	return nil
}
