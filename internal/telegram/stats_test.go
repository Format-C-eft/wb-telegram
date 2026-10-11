package telegram

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// connectionLog запоминает события WithOnConnection.
type connectionLog struct {
	mu     sync.Mutex
	events []string
}

// add — обработчик WithOnConnection.
func (l *connectionLog) add(ok bool, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if ok {
		l.events = append(l.events, "up")

		return
	}

	l.events = append(l.events, "down:"+reason)
}

// snapshot возвращает копию событий.
func (l *connectionLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.events...)
}

// TestOnConnectionAndStats проверяет события связи и счётчики клиента.
func TestOnConnectionAndStats(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	api.queueSendStatus("500", "500", "ok", "403")

	log := &connectionLog{}
	client := startClient(t, api, func(context.Context, Message) {}, WithOnConnection(log.add), withSleep(noSleep))

	_ = client.Send(100, "x")
	_ = client.Send(100, "rejected")

	waitFor(t, "both messages processed", func() bool {
		stats := client.Stats()

		return stats.Sent == 1 && stats.Dropped == 1 && stats.Queued == 0
	})

	events := log.snapshot()
	if len(events) < 2 || !strings.HasPrefix(events[0], "down:") || events[len(events)-1] != "up" {
		t.Fatalf("events = %q, want down first and up last", events)
	}

	if strings.Contains(events[0], testToken) || events[0] == "down:" {
		t.Fatalf("down reason = %q, want masked non-empty reason", events[0])
	}

	if !client.Stats().Connected {
		t.Fatal("Stats().Connected = false after recovery")
	}
}
