package telegram

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestQueueDropsOldest проверяет выброс самого старого сообщения при переполнении.
func TestQueueDropsOldest(t *testing.T) {
	t.Parallel()

	q := newQueue(2)

	for _, text := range []string{"a", "b"} {
		if dropped, ok := q.push(outgoing{text: text}); dropped || !ok {
			t.Fatalf("push %s: dropped=%v ok=%v", text, dropped, ok)
		}
	}

	if dropped, ok := q.push(outgoing{text: "c"}); !dropped || !ok {
		t.Fatalf("push c: dropped=%v ok=%v", dropped, ok)
	}

	ctx := context.Background()

	for _, want := range []string{"b", "c"} {
		item, ok := q.pop(ctx)
		if !ok || item.text != want {
			t.Fatalf("pop = %q, %v; want %q", item.text, ok, want)
		}
	}
}

// TestQueueCloseDrains проверяет, что закрытая очередь отдаёт остаток и затем завершается.
func TestQueueCloseDrains(t *testing.T) {
	t.Parallel()

	q := newQueue(5)
	q.push(outgoing{text: "a"})
	q.close()

	if _, ok := q.push(outgoing{text: "b"}); ok {
		t.Fatal("push after close must fail")
	}

	ctx := context.Background()

	if item, ok := q.pop(ctx); !ok || item.text != "a" {
		t.Fatalf("pop = %q, %v", item.text, ok)
	}

	if _, ok := q.pop(ctx); ok {
		t.Fatal("pop on closed empty queue must return false")
	}
}

// TestQueuePopWaits проверяет ожидание элемента и отмену по ctx.
func TestQueuePopWaits(t *testing.T) {
	t.Parallel()

	q := newQueue(5)

	go func() {
		time.Sleep(50 * time.Millisecond)
		q.push(outgoing{text: "late"})
	}()

	if item, ok := q.pop(context.Background()); !ok || item.text != "late" {
		t.Fatalf("pop = %q, %v", item.text, ok)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, ok := q.pop(ctx); ok {
		t.Fatal("pop must return false after ctx timeout")
	}
}

// TestTruncate проверяет обрезку длинных сообщений по символам, а не байтам.
func TestTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		text      string
		wantRunes int
		wantTail  string
	}{
		{name: "short", text: "привет", wantRunes: 6, wantTail: "привет"},
		{name: "exact", text: strings.Repeat("я", MaxMessageRunes), wantRunes: MaxMessageRunes, wantTail: "я"},
		{name: "long", text: strings.Repeat("я", MaxMessageRunes+10), wantRunes: MaxMessageRunes, wantTail: "…"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := truncate(tt.text)
			if utf8.RuneCountInString(got) != tt.wantRunes || !strings.HasSuffix(got, tt.wantTail) {
				t.Fatalf("truncate: %d runes, tail %q", utf8.RuneCountInString(got), got[len(got)-3:])
			}
		})
	}
}

// TestLimiter проверяет интервалы в один чат и общий интервал.
func TestLimiter(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	l := newLimiter(time.Second, 100*time.Millisecond, func() time.Time { return now })

	if d := l.delay(1); d != 0 {
		t.Fatalf("first delay = %v", d)
	}

	l.record(1)

	if d := l.delay(1); d != time.Second {
		t.Fatalf("same chat delay = %v, want 1s", d)
	}

	if d := l.delay(2); d != 100*time.Millisecond {
		t.Fatalf("other chat delay = %v, want 100ms", d)
	}

	now = now.Add(time.Second)

	if d := l.delay(1); d != 0 {
		t.Fatalf("delay after 1s = %v", d)
	}

	l.record(2)

	if _, ok := l.lastChat[1]; ok || len(l.lastChat) != 1 {
		t.Fatalf("stale chats are not pruned: %v", l.lastChat)
	}
}
