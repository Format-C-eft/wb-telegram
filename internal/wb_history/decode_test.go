package wb_history

import (
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// historyQuery возвращает открытый интервал source fixtures.
func historyQuery() Query {
	return Query{Device: "boiler", Control: "temperature", Start: time.Unix(1794268800, 0), End: time.Unix(1794355200, 0)}
}

// TestDecodeFixtures проверяет обезличенные wire-ответы wb-mqtt-db 2.11.0.
func TestDecodeFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		want  error
		count int
	}{{"normal", nil, 1}, {"empty", nil, 0}, {"overflow", ErrTooManyData, 0}, {"timeout", ErrServiceTimeout, 0}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			payload, errRead := os.ReadFile("testdata/" + test.name + ".json")
			if errRead != nil {
				t.Fatal(errRead)
			}

			points, errDecode := decode(payload, "1", historyQuery())
			if !errors.Is(errDecode, test.want) {
				t.Fatalf("error=%v want %v", errDecode, test.want)
			}

			if errDecode != nil {
				return
			}

			if len(points) != test.count {
				t.Fatalf("points=%d", len(points))
			}

			if len(points) > 0 && (!points[0].Valid || points[0].Value != 22.5 || points[0].Time.Nanosecond() != 125000000 || !points[0].HasMin || points[0].Min != 21.9 || points[0].Max != 23.1) {
				t.Fatalf("point=%+v", points[0])
			}
		})
	}
}

// TestDecodeInvalid проверяет несовместимые и нечисловые ответы.
func TestDecodeInvalid(t *testing.T) {
	t.Parallel()

	base := `{"id":"1","result":{"values":[{"c":42,"i":1,"t":1794268801.125,"v":"2"}],"has_more":false}}`
	tests := []struct {
		name, payload string
		want          error
		invalid       bool
	}{
		{"json", "{", ErrInvalidResponse, false},
		{"version", strings.Replace(base, `"values":`, `"ver":2,"values":`, 1), ErrUnsupported, false},
		{"id", strings.Replace(base, `"id":"1"`, `"id":1.1`, 1), ErrInvalidResponse, false},
		{"uid", strings.Replace(base, `"i":1`, `"i":-1`, 1), ErrInvalidResponse, false},
		{"time", strings.Replace(base, "1794268801.125", "1794268800", 1), ErrInvalidResponse, false},
		{"missing", strings.Replace(base, `,"has_more":false`, "", 1), nil, false},
		{"unrelated", strings.Replace(base, `"id":"1"`, `"id":"2"`, 1), errUnrelated, false},
		{"text", strings.Replace(base, `"v":"2"`, `"v":"error"`, 1), nil, true},
		{"null", strings.Replace(base, `"v":"2"`, `"v":null`, 1), nil, true},
		{"nan", strings.Replace(base, `"v":"2"`, `"v":"NaN"`, 1), nil, true},
		{"range", strings.Replace(base, `"v":"2"`, `"v":"2","min":"3","max":"1"`, 1), nil, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			points, errDecode := decode([]byte(test.payload), "1", historyQuery())
			if !errors.Is(errDecode, test.want) {
				t.Fatalf("error=%v want %v", errDecode, test.want)
			}

			if errDecode != nil {
				return
			}

			if len(points) != 1 || points[0].Valid == test.invalid {
				t.Fatalf("points=%+v", points)
			}
		})
	}
}

// TestUintID проверяет отсутствие float-округления uint64.
func TestUintID(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{`"18446744073709551615"`, `18446744073709551615`} {
		id, ok := uintID([]byte(raw))
		if !ok || id != "18446744073709551615" {
			t.Fatalf("id=%q ok=%v", id, ok)
		}
	}

	if finite(math.Inf(1)) || finite(math.NaN()) {
		t.Fatal("non-finite accepted")
	}
}
