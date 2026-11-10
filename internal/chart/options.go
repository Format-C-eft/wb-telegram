package chart

import "time"

// Option настраивает построитель графиков.
type Option func(*options)
type options struct{ historyBudget time.Duration }

// Validate проверяет предел времени чтения истории.
func (o options) Validate() error {
	if o.historyBudget <= 0 || o.historyBudget > 10*time.Second {
		return ErrInvalidArgument
	}

	return nil
}

// WithHistoryBudget задаёт общий бюджет чтения всех каналов.
func WithHistoryBudget(budget time.Duration) Option {
	return func(o *options) { o.historyBudget = budget }
}
