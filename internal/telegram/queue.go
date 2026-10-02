package telegram

import (
	"context"
	"sync"
)

// outgoing — сообщение в очереди отправки.
type outgoing struct {
	chatID int64
	text   string
}

// queue — ограниченная FIFO-очередь с выбросом самого старого при переполнении.
type queue struct {
	mu     sync.Mutex
	items  []outgoing
	size   int
	closed bool
	notify chan struct{}
}

// newQueue создаёт очередь ёмкостью size.
func newQueue(size int) *queue {
	return &queue{size: size, notify: make(chan struct{}, 1)}
}

// push добавляет сообщение в конец; dropped — выброшено ли самое старое; ok=false — очередь закрыта.
func (q *queue) push(item outgoing) (dropped, ok bool) {
	q.mu.Lock()

	if q.closed {
		q.mu.Unlock()

		return false, false
	}

	if len(q.items) >= q.size {
		q.items = q.items[1:]
		dropped = true
	}

	q.items = append(q.items, item)

	q.mu.Unlock()

	q.signal()

	return dropped, true
}

// pop ждёт и забирает первое сообщение; false — очередь закрыта и пуста или ctx отменён.
func (q *queue) pop(ctx context.Context) (outgoing, bool) {
	for {
		q.mu.Lock()

		if len(q.items) > 0 {
			item := q.items[0]
			q.items = q.items[1:]

			q.mu.Unlock()

			return item, true
		}

		closed := q.closed

		q.mu.Unlock()

		if closed {
			return outgoing{}, false
		}

		select {
		case <-ctx.Done():
			return outgoing{}, false
		case <-q.notify:
		}
	}
}

// len возвращает число сообщений в очереди.
func (q *queue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.items)
}

// close запрещает новые сообщения; оставшиеся ещё можно забрать.
func (q *queue) close() {
	q.mu.Lock()

	q.closed = true

	q.mu.Unlock()

	q.signal()
}

// signal будит ожидающий pop.
func (q *queue) signal() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}
