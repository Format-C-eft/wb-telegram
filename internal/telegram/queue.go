package telegram

import (
	"context"
	"sync"
)

// outgoing - сообщение в очереди отправки.
type outgoing struct {
	chatID   int64
	text     string
	delivery Delivery
}

// queue - ограниченная FIFO-очередь с выбросом самого старого при переполнении.
type queue struct {
	mu     sync.Mutex
	items  []outgoing
	size   int
	bytes  int
	closed bool
	notify chan struct{}
}

// newQueue создаёт очередь ёмкостью size.
func newQueue(size int) *queue {
	return &queue{size: size, notify: make(chan struct{}, 1)}
}

// push добавляет сообщение в конец; dropped - выброшено ли самое старое; ok=false - очередь закрыта.
func (q *queue) push(item outgoing) (dropped, ok bool) {
	count, accepted := q.pushDelivery(item)

	return count > 0, accepted
}

// pushDelivery добавляет элемент и возвращает число вытесненных сообщений.
func (q *queue) pushDelivery(item outgoing) (int, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return 0, false
	}

	count := 0

	for len(q.items) >= q.size || q.bytes+len(item.delivery.PNG) > maxQueuePNGBytes {
		q.bytes -= len(q.items[0].delivery.PNG)
		q.items[0] = outgoing{}
		q.items = q.items[1:]
		count++
	}

	q.items = append(q.items, item)
	q.bytes += len(item.delivery.PNG)
	q.signal()

	return count, true
}

// pop ждёт и забирает первое сообщение; false - очередь закрыта и пуста или ctx отменён.
func (q *queue) pop(ctx context.Context) (outgoing, bool) {
	for {
		q.mu.Lock()

		if len(q.items) > 0 {
			item := q.items[0]
			q.bytes -= len(item.delivery.PNG)
			q.items[0] = outgoing{}
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

// release освобождает данные оставшихся сообщений закрытой очереди.
func (q *queue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()

	for index := range q.items {
		q.items[index] = outgoing{}
	}

	q.bytes = 0
}
