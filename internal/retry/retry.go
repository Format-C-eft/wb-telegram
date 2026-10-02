// Package retry — общие примитивы повторов: ожидание с отменой и экспоненциальная пауза.
package retry

import (
	"context"
	"time"
)

// Sleep ждёт d или отмены ctx; возвращает false, если ctx отменён.
func Sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Delay возвращает паузу перед повтором номер attempt (с 0): base·2^attempt, не больше maxDelay.
func Delay(attempt int, base, maxDelay time.Duration) time.Duration {
	delay := min(base, maxDelay)

	for range attempt {
		if delay >= maxDelay || delay > maxDelay/2 {
			return maxDelay
		}

		delay *= 2
	}

	return delay
}
