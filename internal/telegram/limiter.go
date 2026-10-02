package telegram

import (
	"time"
)

// limiter считает паузы между отправками; используется только из цикла отправки (одна горутина).
type limiter struct {
	perChat    time.Duration
	global     time.Duration
	now        func() time.Time
	lastGlobal time.Time
	lastChat   map[int64]time.Time
}

// newLimiter создаёт ограничитель с интервалами perChat и global.
func newLimiter(perChat, global time.Duration, now func() time.Time) *limiter {
	return &limiter{perChat: perChat, global: global, now: now, lastChat: map[int64]time.Time{}}
}

// delay возвращает, сколько ждать до отправки в chatID.
func (l *limiter) delay(chatID int64) time.Duration {
	next := l.lastGlobal.Add(l.global)

	if last, ok := l.lastChat[chatID]; ok {
		chatNext := last.Add(l.perChat)
		if chatNext.After(next) {
			next = chatNext
		}
	}

	wait := next.Sub(l.now())
	if wait < 0 {
		return 0
	}

	return wait
}

// record отмечает отправку в chatID и забывает чаты, интервал для которых уже истёк.
func (l *limiter) record(chatID int64) {
	now := l.now()

	for id, last := range l.lastChat {
		if now.Sub(last) >= l.perChat {
			delete(l.lastChat, id)
		}
	}

	l.lastGlobal = now
	l.lastChat[chatID] = now
}
