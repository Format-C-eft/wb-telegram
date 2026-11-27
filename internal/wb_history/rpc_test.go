package wb_history

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/wb_history/mock"
)

// TestReadCancellation проверяет отмену и освобождение единственного pending.
func TestReadCancellation(t *testing.T) {
	t.Parallel()

	fixture := newHistoryFixture(t)
	fixture.run(t)
	requests := fixture.publish(t, completedToken(fixture.ctrl, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)

	go func() { _, errRead := fixture.client.Read(ctx, historyQuery()); result <- errRead }()

	<-requests
	cancel()

	if !errors.Is(<-result, ErrCanceled) {
		t.Fatal("cancel not propagated")
	}

	fixture.client.mu.Lock()
	active := fixture.client.pending != nil
	fixture.client.mu.Unlock()

	if active {
		t.Fatal("pending leaked")
	}

	short, shortCancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer shortCancel()

	_, errShort := fixture.client.Read(short, historyQuery())
	if !errors.Is(errShort, ErrTimeout) {
		t.Fatal(errShort)
	}
}

// TestStopPending проверяет остановку во время ожидания отсутствующей базы.
func TestStopPending(t *testing.T) {
	t.Parallel()

	fixture := newHistoryFixture(t)
	fixture.run(t)
	requests := fixture.publish(t, completedToken(fixture.ctrl, nil))
	result := make(chan error, 1)

	go func() { _, errRead := fixture.client.Read(context.Background(), historyQuery()); result <- errRead }()

	<-requests

	errStop := fixture.client.Stop()
	if errStop != nil {
		t.Fatal(errStop)
	}

	if !errors.Is(<-result, ErrClosed) {
		t.Fatal("stop did not close query")
	}

	errAgain := fixture.client.Run(context.Background())
	if !errors.Is(errAgain, ErrClosed) {
		t.Fatal(errAgain)
	}
}

// TestBrokerUnavailableStart проверяет неблокирующий запуск без CONNACK.
func TestBrokerUnavailableStart(t *testing.T) {
	t.Parallel()

	fixture := newHistoryFixture(t)
	token := mock.NewMockToken(fixture.ctrl)
	fixture.mqtt.EXPECT().Connect().Return(token)

	started := time.Now()

	errRun := fixture.client.Run(context.Background())
	if errRun != nil {
		t.Fatal(errRun)
	}

	if time.Since(started) > time.Second {
		t.Fatal("Run waited for broker")
	}

	_, errRead := fixture.client.Read(context.Background(), historyQuery())
	if !errors.Is(errRead, ErrUnavailable) {
		t.Fatal(errRead)
	}
}

// TestReconnectNoReplay проверяет завершение запроса при обрыве без воспроизведения.
func TestReconnectNoReplay(t *testing.T) {
	t.Parallel()

	fixture := newHistoryFixture(t)
	fixture.run(t)
	requests := fixture.publish(t, completedToken(fixture.ctrl, nil))
	result := make(chan error, 1)

	go func() { _, errRead := fixture.client.Read(context.Background(), historyQuery()); result <- errRead }()

	<-requests
	fixture.config.OnReconnecting(fixture.mqtt, fixture.config)

	if !errors.Is(<-result, ErrUnavailable) {
		t.Fatal("disconnect did not fail query")
	}

	fixture.expectSubscribe(fixture.subscription(1))
	fixture.config.OnConnect(fixture.mqtt)

	if !fixture.client.Ready() {
		t.Fatal("reconnect did not resubscribe")
	}

	fixture.client.mu.Lock()
	active := fixture.client.pending != nil
	sequence := fixture.client.sequence
	fixture.client.mu.Unlock()

	if active || sequence != 1 {
		t.Fatal("request was replayed")
	}
}

// TestReadPublishError проверяет ошибку подтверждения публикации.
func TestReadPublishError(t *testing.T) {
	t.Parallel()

	fixture := newHistoryFixture(t)
	fixture.run(t)
	fixture.publish(t, completedToken(fixture.ctrl, errors.New("publish failed")))

	_, errRead := fixture.client.Read(context.Background(), historyQuery())
	if !errors.Is(errRead, ErrUnavailable) {
		t.Fatal(errRead)
	}
}

// TestReadDeadline проверяет истечение срока ожидания ответа после PUBACK.
func TestReadDeadline(t *testing.T) {
	t.Parallel()

	fixture := newHistoryFixture(t)
	fixture.run(t)
	fixture.publish(t, completedToken(fixture.ctrl, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 1100*time.Millisecond)
	defer cancel()

	_, errRead := fixture.client.Read(ctx, historyQuery())
	if !errors.Is(errRead, ErrTimeout) {
		t.Fatal(errRead)
	}
}

// TestReadIgnoredReplies проверяет фильтрацию retained и чужого ID.
func TestReadIgnoredReplies(t *testing.T) {
	t.Parallel()

	for _, retained := range []bool{true, false} {
		t.Run(map[bool]string{true: "retained", false: "unknown_id"}[retained], func(t *testing.T) {
			t.Parallel()

			fixture := newHistoryFixture(t)
			fixture.run(t)
			requests := fixture.publish(t, completedToken(fixture.ctrl, nil))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			result := make(chan error, 1)

			go func() { _, errRead := fixture.client.Read(ctx, historyQuery()); result <- errRead }()

			<-requests

			id := "2"
			if retained {
				id = "1"
			}

			fixture.deliver(`{"id":"`+id+`","result":{"values":[],"has_more":false}}`, retained)

			select {
			case errRead := <-result:
				t.Fatalf("irrelevant reply completed query: %v", errRead)
			case <-time.After(10 * time.Millisecond):
			}

			cancel()

			if !errors.Is(<-result, ErrCanceled) {
				t.Fatal("irrelevant reply replaced cancellation")
			}
		})
	}
}

// TestReadOversizedReply проверяет ограничение размера до копирования ответа.
func TestReadOversizedReply(t *testing.T) {
	t.Parallel()

	fixture := newHistoryFixture(t)
	fixture.run(t)
	requests := fixture.publish(t, completedToken(fixture.ctrl, nil))
	result := make(chan error, 1)

	go func() { _, errRead := fixture.client.Read(context.Background(), historyQuery()); result <- errRead }()

	<-requests
	fixture.deliver(strings.Repeat(" ", maxResponseBytes+1), false)

	if !errors.Is(<-result, ErrTooManyData) {
		t.Fatal("oversized reply accepted")
	}
}
