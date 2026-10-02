package retry

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestDelay проверяет экспоненциальный рост паузы и её ограничение сверху.
func TestDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		attempt  int
		base     time.Duration
		maxDelay time.Duration
		want     time.Duration
	}{
		{name: "first attempt is base", attempt: 0, base: time.Second, maxDelay: time.Minute, want: time.Second},
		{name: "second doubles", attempt: 1, base: time.Second, maxDelay: time.Minute, want: 2 * time.Second},
		{name: "fourth", attempt: 3, base: time.Second, maxDelay: time.Minute, want: 8 * time.Second},
		{name: "capped", attempt: 10, base: time.Second, maxDelay: time.Minute, want: time.Minute},
		{name: "exactly max", attempt: 2, base: time.Second, maxDelay: 4 * time.Second, want: 4 * time.Second},
		{name: "huge attempt does not overflow", attempt: math.MaxInt32, base: time.Second, maxDelay: time.Minute, want: time.Minute},
		{name: "huge base does not overflow", attempt: 5, base: math.MaxInt64 / 2, maxDelay: math.MaxInt64 - 1, want: math.MaxInt64 - 1},
		{name: "negative attempt is base", attempt: -1, base: time.Second, maxDelay: time.Minute, want: time.Second},
		{name: "base above max is capped", attempt: 0, base: time.Minute, maxDelay: time.Second, want: time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := Delay(tt.attempt, tt.base, tt.maxDelay); got != tt.want {
				t.Fatalf("Delay(%d, %v, %v) = %v, want %v", tt.attempt, tt.base, tt.maxDelay, got, tt.want)
			}
		})
	}
}

// TestSleep проверяет ожидание и прерывание по отмене контекста.
func TestSleep(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		d    time.Duration
		want bool
	}{
		{name: "elapsed", ctx: context.Background(), d: time.Millisecond, want: true},
		{name: "canceled", ctx: canceled, d: time.Hour, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := Sleep(tt.ctx, tt.d); got != tt.want {
				t.Fatalf("Sleep = %v, want %v", got, tt.want)
			}
		})
	}
}
