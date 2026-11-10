package chart

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/wb_history"
)

// TestNormalize проверяет сортировку, дубликаты, границы и ошибочные агрегаты.
func TestNormalize(t *testing.T) {
	t.Parallel()

	start := time.Unix(1000, 0)
	end := start.Add(time.Hour)
	middle := start.Add(time.Minute)
	points := []wb_history.Point{
		{Time: middle.Add(time.Minute), Value: 3, Valid: true},
		{Time: middle, Value: 1, Valid: true},
		{Time: middle, Value: 2, Valid: true},
		{Time: start, Value: 99, Valid: true},
		{Time: end, Value: 99, Valid: true},
		{Time: middle.Add(2 * time.Minute), Value: 2, Min: 3, Max: 1, HasMin: true, HasMax: true, Valid: true},
		{Time: middle.Add(3 * time.Minute), Value: math.NaN(), Valid: true},
	}

	normalized, errNormalize := normalize(points, start, end)
	if errNormalize != nil {
		t.Fatal(errNormalize)
	}

	if len(normalized) != 4 || normalized[0].Value != 2 || normalized[1].Value != 3 || normalized[2].Valid || normalized[3].Valid {
		t.Fatalf("points=%+v", normalized)
	}

	if points[0].Value != 3 {
		t.Fatal("mutated source")
	}
}

// TestScale проверяет конечность шкалы практически постоянных и крайних рядов.
func TestScale(t *testing.T) {
	t.Parallel()

	for _, value := range []float64{0, math.Copysign(0, -1), 1e-300, -3, 1e12, -1e12} {
		low, high := bounds([][]wb_history.Point{{{Value: value, Valid: true}}})
		if !finite(low) || !finite(high) || low >= value || high <= value {
			t.Fatalf("value=%g bounds=%g..%g", value, low, high)
		}
	}

	_, errRange := normalize([]wb_history.Point{{Time: time.Unix(1001, 0), Value: -math.MaxFloat64, Valid: true}}, time.Unix(1000, 0), time.Unix(2000, 0))
	if !errors.Is(errRange, ErrRange) {
		t.Fatal(errRange)
	}
}
