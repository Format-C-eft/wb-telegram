package wb_history

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"
)

var errUnrelated = errors.New("unrelated response")

type envelope struct {
	ID     json.RawMessage `json:"id"`
	Error  *rpcError       `json:"error"`
	Result *wireResult     `json:"result"`
}
type rpcError struct {
	Code int `json:"code"`
}
type wireResult struct {
	Version *int            `json:"ver"`
	Values  json.RawMessage `json:"values"`
	HasMore *bool           `json:"has_more"`
}
type wirePoint struct {
	Channel json.RawMessage `json:"c"`
	UID     json.RawMessage `json:"i"`
	Time    *float64        `json:"t"`
	Value   json.RawMessage `json:"v"`
	Min     json.RawMessage `json:"min"`
	Max     json.RawMessage `json:"max"`
}

// decode проверяет wire-контракт ответа одного канала.
func decode(payload []byte, id string, query Query) ([]Point, error) {
	if len(payload) > maxResponseBytes {
		return nil, ErrTooManyData
	}

	var message envelope

	errJSON := json.Unmarshal(payload, &message)
	if errJSON != nil {
		return nil, ErrInvalidResponse
	}

	responseID, ok := uintID(message.ID)
	if !ok {
		return nil, ErrInvalidResponse
	}

	if responseID != id {
		return nil, errUnrelated
	}

	if message.Error != nil {
		if message.Error.Code == -32100 {
			return nil, ErrServiceTimeout
		}

		return nil, ErrUnavailable
	}

	if message.Result == nil {
		return nil, ErrUnsupported
	}

	if message.Result.Version != nil && *message.Result.Version != 1 {
		return nil, ErrUnsupported
	}

	if message.Result.HasMore != nil && *message.Result.HasMore {
		return nil, ErrTooManyData
	}

	return decodeValues(message.Result.Values, query)
}

// decodeValues читает не более тысячи записей одного канала.
func decodeValues(raw json.RawMessage, query Query) ([]Point, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))

	token, errToken := decoder.Token()
	if errToken != nil {
		return nil, ErrInvalidResponse
	}

	if token != json.Delim('[') {
		return nil, ErrInvalidResponse
	}

	points := make([]Point, 0)
	channel := ""

	for decoder.More() {
		if len(points) >= maxPoints {
			return nil, ErrTooManyData
		}

		var record wirePoint

		errRecord := decoder.Decode(&record)
		if errRecord != nil {
			return nil, ErrInvalidResponse
		}

		current, channelOK := uintID(record.Channel)
		if !channelOK || (channel != "" && channel != current) {
			return nil, ErrInvalidResponse
		}

		channel = current

		point, errPoint := decodePoint(record, query)
		if errPoint != nil {
			return nil, errPoint
		}

		points = append(points, point)
	}

	return points, nil
}

// decodePoint переводит запись в измерение или явный разрыв.
func decodePoint(record wirePoint, query Query) (Point, error) {
	_, uidOK := uintID(record.UID)
	if !uidOK || record.Time == nil || !finite(*record.Time) {
		return Point{}, ErrInvalidResponse
	}

	if *record.Time <= secondsUnix(query.Start) || *record.Time >= secondsUnix(query.End) {
		return Point{}, ErrInvalidResponse
	}

	whole, fraction := math.Modf(*record.Time)
	if whole < math.MinInt64 || whole >= math.MaxInt64 {
		return Point{}, ErrInvalidResponse
	}

	point := Point{Time: time.Unix(int64(whole), int64(math.Round(fraction*1e9)))}
	point.Value, point.Valid = numeric(record.Value)
	point.Min, point.HasMin = numeric(record.Min)

	point.Max, point.HasMax = numeric(record.Max)
	if (present(record.Min) && !point.HasMin) || (present(record.Max) && !point.HasMax) {
		point.Valid = false
	}

	if !validAggregate(point) {
		point.Valid = false
	}

	return point, nil
}

// validAggregate проверяет согласованность среднего с границами.
func validAggregate(point Point) bool {
	return (!point.HasMin || point.Value >= point.Min) && (!point.HasMax || point.Value <= point.Max) && (!point.HasMin || !point.HasMax || point.Min <= point.Max)
}

// uintID принимает десятичную строку или точный JSON uint64.
func uintID(raw json.RawMessage) (string, bool) {
	value := string(raw)
	if len(value) > 0 && value[0] == '"' {
		errString := json.Unmarshal(raw, &value)
		if errString != nil {
			return "", false
		}
	}

	if value == "" {
		return "", false
	}

	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return "", false
		}
	}

	number, errNumber := strconv.ParseUint(value, 10, 64)
	if errNumber != nil {
		return "", false
	}

	return strconv.FormatUint(number, 10), true
}

// present отличает отсутствующее значение от некорректного.
func present(raw json.RawMessage) bool { return len(raw) > 0 && !bytes.Equal(raw, []byte("null")) }

// numeric читает конечное число без подмены отсутствующего значения нулём.
func numeric(raw json.RawMessage) (float64, bool) {
	if !present(raw) {
		return 0, false
	}

	text := string(raw)
	if text[0] == '"' {
		errString := json.Unmarshal(raw, &text)
		if errString != nil {
			return 0, false
		}
	}

	value, errNumber := strconv.ParseFloat(text, 64)
	if errNumber != nil || !finite(value) {
		return 0, false
	}

	return value, true
}

// finite проверяет конечность числа.
func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
