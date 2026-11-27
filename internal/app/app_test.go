package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/app/mock"
)

// newTestApp создаёт App без подписки на реальные сигналы.
func newTestApp(t *testing.T, opts ...Option) *App {
	t.Helper()

	opts = append(opts, withSignalContext(func() (context.Context, context.CancelFunc) {
		return context.WithCancel(context.Background())
	}))

	a, errNew := New(opts...)
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	return a
}

// TestRunOrder проверяет порядок запуска, обратный порядок остановки и closers.
func TestRunOrder(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	a := newTestApp(t)
	first := mock.NewMockComponent(ctrl)
	graceful := mock.NewMockGracefulComponent(ctrl)
	last := mock.NewMockComponent(ctrl)
	started := make(chan struct{})
	stopped := false
	closers := 0

	gomock.InOrder(
		first.EXPECT().Run(gomock.Any()).Return(nil),
		graceful.EXPECT().Run(gomock.Any()).Return(nil),
		last.EXPECT().Run(gomock.Any()).DoAndReturn(func(context.Context) error {
			close(started)

			return nil
		}),
		last.EXPECT().Stop().Return(nil),
		graceful.EXPECT().Shutdown(gomock.Any()).Return(nil),
		first.EXPECT().Stop().DoAndReturn(func() error {
			stopped = true

			return nil
		}),
	)

	a.AddComponent(first, graceful, last)
	a.AddCloser(func() error {
		if !stopped || closers != 1 {
			t.Error("first closer ran before components or second closer")
		}

		closers++

		return nil
	}, func() error {
		if !stopped || closers != 0 {
			t.Error("second closer ran before components")
		}

		closers++

		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- a.Run(ctx) }()

	<-started
	cancel()

	errRun := <-done
	if errRun != nil || closers != 2 {
		t.Fatalf("Run error=%v closers=%d", errRun, closers)
	}
}

// TestRunRollbackOnStartError проверяет откат успешно запущенных компонентов.
func TestRunRollbackOnStartError(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	a := newTestApp(t)
	errBoom := errors.New("boom")
	first := mock.NewMockComponent(ctrl)
	failed := mock.NewMockComponent(ctrl)
	remaining := mock.NewMockComponent(ctrl)

	gomock.InOrder(
		first.EXPECT().Run(gomock.Any()).Return(nil),
		failed.EXPECT().Run(gomock.Any()).Return(errBoom),
		first.EXPECT().Stop().Return(nil),
	)

	a.AddComponent(first, failed, remaining)

	errRun := a.Run(context.Background())
	if !errors.Is(errRun, errBoom) {
		t.Fatalf("Run error = %v, want boom", errRun)
	}
}

// TestRunTerminateCause проверяет возвращение первой причины Terminate.
func TestRunTerminateCause(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	a := newTestApp(t)
	errCause := errors.New("token rejected")
	component := mock.NewMockComponent(ctrl)
	started := make(chan struct{})

	component.EXPECT().Run(gomock.Any()).DoAndReturn(func(context.Context) error {
		close(started)

		return nil
	})
	component.EXPECT().Stop().Return(nil)
	a.AddComponent(component)

	done := make(chan error, 1)

	go func() { done <- a.Run(context.Background()) }()

	<-started
	a.Terminate(errCause)
	a.Terminate(errors.New("ignored second cause"))

	select {
	case errRun := <-done:
		if !errors.Is(errRun, errCause) {
			t.Fatalf("Run error = %v, want cause", errRun)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Terminate")
	}
}

// TestRunErrors проверяет nil-компонент и повторный запуск.
func TestRunErrors(t *testing.T) {
	t.Parallel()

	t.Run("nil component", func(t *testing.T) {
		t.Parallel()

		a := newTestApp(t)
		a.AddComponent(nil)

		errRun := a.Run(context.Background())
		if !errors.Is(errRun, ErrNilComponent) {
			t.Fatalf("Run error = %v, want ErrNilComponent", errRun)
		}
	})

	t.Run("already running", func(t *testing.T) {
		t.Parallel()

		ctrl := gomock.NewController(t)
		a := newTestApp(t)
		component := mock.NewMockComponent(ctrl)
		started := make(chan struct{})

		component.EXPECT().Run(gomock.Any()).DoAndReturn(func(context.Context) error {
			close(started)

			return nil
		})
		component.EXPECT().Stop().Return(nil)
		a.AddComponent(component)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)

		go func() { done <- a.Run(ctx) }()

		<-started

		errRun := a.Run(ctx)
		if !errors.Is(errRun, ErrAlreadyRunning) {
			t.Fatalf("second Run error = %v, want ErrAlreadyRunning", errRun)
		}

		cancel()

		if errFirst := <-done; errFirst != nil {
			t.Fatal(errFirst)
		}
	})
}

// TestNewValidatesOptions проверяет отказ на неположительном таймауте остановки.
func TestNewValidatesOptions(t *testing.T) {
	t.Parallel()

	_, errNew := New(WithShutdownTimeout(0))
	if !errors.Is(errNew, ErrInvalidOption) {
		t.Fatalf("New error = %v, want ErrInvalidOption", errNew)
	}
}

// TestRunCancelsComponentContext проверяет отмену контекста после Terminate.
func TestRunCancelsComponentContext(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	a := newTestApp(t)
	component := mock.NewMockComponent(ctrl)
	started := make(chan context.Context, 1)

	component.EXPECT().Run(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		started <- ctx

		return nil
	})
	component.EXPECT().Stop().Return(nil)
	a.AddComponent(component)

	done := make(chan error, 1)

	go func() { done <- a.Run(context.Background()) }()

	ctx := <-started
	errCause := errors.New("stop")
	a.Terminate(errCause)

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("component context was not canceled")
	}

	if errRun := <-done; !errors.Is(errRun, errCause) {
		t.Fatal(errRun)
	}
}

// TestRunTerminateDuringStart проверяет откат после Terminate во время запуска.
func TestRunTerminateDuringStart(t *testing.T) {
	t.Parallel()

	a := newTestApp(t)
	errCause := errors.New("cause")

	expectInterruptedStart(t, a, func() { a.Terminate(errCause) })

	errRun := a.Run(context.Background())
	if !errors.Is(errRun, errCause) {
		t.Fatalf("Run error = %v, want cause", errRun)
	}
}

// TestRunCancelDuringStart проверяет откат при отмене внешнего контекста.
func TestRunCancelDuringStart(t *testing.T) {
	t.Parallel()

	a := newTestApp(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	expectInterruptedStart(t, a, cancel)

	errRun := a.Run(ctx)
	if errRun != nil {
		t.Fatalf("Run: %v", errRun)
	}
}

// expectInterruptedStart задаёт прерывание второго запуска и обратный откат.
func expectInterruptedStart(t *testing.T, a *App, interrupt func()) {
	t.Helper()

	ctrl := gomock.NewController(t)
	first := mock.NewMockComponent(ctrl)
	slow := mock.NewMockComponent(ctrl)
	remaining := mock.NewMockComponent(ctrl)

	gomock.InOrder(
		first.EXPECT().Run(gomock.Any()).Return(nil),
		slow.EXPECT().Run(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
			interrupt()
			<-ctx.Done()

			return nil
		}),
		slow.EXPECT().Stop().Return(nil),
		first.EXPECT().Stop().Return(nil),
	)

	a.AddComponent(first, slow, remaining)
}

// TestRunShutdownTimeout проверяет срок мягкой остановки и выполнение closers.
func TestRunShutdownTimeout(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	a := newTestApp(t, WithShutdownTimeout(10*time.Millisecond))
	component := mock.NewMockGracefulComponent(ctrl)
	closed := false

	component.EXPECT().Run(gomock.Any()).DoAndReturn(func(context.Context) error {
		a.Terminate(nil)

		return nil
	})
	component.EXPECT().Shutdown(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("Shutdown has no deadline")
		}

		<-ctx.Done()

		return ctx.Err()
	})

	a.AddComponent(component)
	a.AddCloser(func() error {
		closed = true

		return nil
	})

	errRun := a.Run(context.Background())
	if !errors.Is(errRun, context.DeadlineExceeded) || !closed {
		t.Fatalf("Run error=%v closer=%v", errRun, closed)
	}
}
