package chart

import (
	"math"
	"slices"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/wb_history"
)

// normalize сортирует точки и сохраняет последнюю запись каждого timestamp.
func normalize(points []wb_history.Point, start, end time.Time) ([]wb_history.Point, error) {
	if len(points) > 1000 {
		return nil, ErrTooLarge
	}

	result := slices.Clone(points)
	slices.SortStableFunc(result, func(left, right wb_history.Point) int { return left.Time.Compare(right.Time) })

	count := 0

	for _, point := range result {
		if !point.Time.After(start) || !point.Time.Before(end) {
			continue
		}

		errPoint := checkPoint(&point)
		if errPoint != nil {
			return nil, errPoint
		}

		if count > 0 && result[count-1].Time.Equal(point.Time) {
			result[count-1] = point

			continue
		}

		result[count] = point
		count++
	}

	return result[:count], nil
}

// finite проверяет возможность безопасного расчёта координаты.
func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

// checkPoint нормализует отдельное измерение и проверяет числовой предел.
func checkPoint(point *wb_history.Point) error {
	if outsideRange(*point) {
		return ErrRange
	}

	if !point.Valid {
		return nil
	}

	if !finitePoint(*point) {
		point.Valid = false
	}

	if (point.HasMin && point.Value < point.Min) || (point.HasMax && point.Value > point.Max) || (point.HasMin && point.HasMax && point.Min > point.Max) {
		point.Valid = false
	}

	return nil
}

// finitePoint проверяет конечность присутствующих значений точки.
func finitePoint(point wb_history.Point) bool {
	return finite(point.Value) && (!point.HasMin || finite(point.Min)) && (!point.HasMax || finite(point.Max))
}

// outsideRange проверяет предел конечных значений измерения.
func outsideRange(point wb_history.Point) bool {
	values := []float64{point.Value}
	if point.HasMin {
		values = append(values, point.Min)
	}

	if point.HasMax {
		values = append(values, point.Max)
	}

	for _, value := range values {
		if finite(value) && math.Abs(value) > 1e12 {
			return true
		}
	}

	return false
}
