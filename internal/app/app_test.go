package app

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// recorder собирает журнал вызовов компонентов в порядке их выполнения.
type recorder struct {
	mu  sync.Mutex
	log []string
}

// add добавляет запись в журнал.
func (r *recorder) add(entry string) {
	r.mu.Lock()
	r.log = append(r.log, entry)
	r.mu.Unlock()
}

// entries возвращает копию журнала.
func (r *recorder) entries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.log)
}

// fakeComponent — компонент, который пишет свои вызовы в recorder.
type fakeComponent struct {
	name    string
	rec     *recorder
	runErr  error
	started chan struct{}
}

// Run записывает запуск и сигнализирует о нём.
func (c *fakeComponent) Run(_ context.Context) error {
	c.rec.add("run " + c.name)

	if c.started != nil {
		close(c.started)
	}

	return c.runErr
}

// Stop записывает остановку.
func (c *fakeComponent) Stop() error {
	c.rec.add("stop " + c.name)

	return nil
}

// gracefulComponent — компонент с context-aware остановкой.
type gracefulComponent struct {
	fakeComponent
}

// Shutdown записывает мягкую остановку.
func (c *gracefulComponent) Shutdown(_ context.Context) error {
	c.rec.add("shutdown " + c.name)

	return nil
}

// newTestApp создаёт App без подписки на реальные сигналы.
func newTestApp(t *testing.T) *App {
	t.Helper()

	a, errNew := New(withSignalContext(func() (context.Context, context.CancelFunc) {
		return context.WithCancel(context.Background())
	}))
	if errNew != nil {
		t.Fatalf("New: %v", errNew)
	}

	return a
}

// TestRunOrder проверяет порядок запуска, обратный порядок остановки и closers.
func TestRunOrder(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	a := newTestApp(t)
	last := &fakeComponent{name: "b", rec: rec, started: make(chan struct{})}

	a.AddComponent(&fakeComponent{name: "a", rec: rec}, &gracefulComponent{fakeComponent{name: "g", rec: rec}}, last)
	a.AddCloser(func() error {
		rec.add("closer")

		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- a.Run(ctx) }()

	<-last.started
	cancel()

	errRun := <-done
	if errRun != nil {
		t.Fatalf("Run: %v", errRun)
	}

	want := []string{"run a", "run g", "run b", "stop b", "shutdown g", "stop a", "closer"}
	if got := rec.entries(); !slices.Equal(got, want) {
		t.Fatalf("log = %v, want %v", got, want)
	}
}

// TestRunRollbackOnStartError проверяет остановку уже запущенных компонентов при ошибке старта.
func TestRunRollbackOnStartError(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	a := newTestApp(t)
	errBoom := errors.New("boom")

	a.AddComponent(&fakeComponent{name: "a", rec: rec}, &fakeComponent{name: "b", rec: rec, runErr: errBoom}, &fakeComponent{name: "c", rec: rec})

	errRun := a.Run(context.Background())
	if !errors.Is(errRun, errBoom) {
		t.Fatalf("Run error = %v, want boom", errRun)
	}

	want := []string{"run a", "run b", "stop a"}
	if got := rec.entries(); !slices.Equal(got, want) {
		t.Fatalf("log = %v, want %v", got, want)
	}
}

// TestRunTerminateCause проверяет, что причина Terminate возвращается из Run.
func TestRunTerminateCause(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	a := newTestApp(t)
	errCause := errors.New("token rejected")
	comp := &fakeComponent{name: "a", rec: rec, started: make(chan struct{})}

	a.AddComponent(comp)

	done := make(chan error, 1)

	go func() { done <- a.Run(context.Background()) }()

	<-comp.started
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

		rec := &recorder{}
		a := newTestApp(t)
		comp := &fakeComponent{name: "a", rec: rec, started: make(chan struct{})}

		a.AddComponent(comp)

		ctx := t.Context()

		go func() { _ = a.Run(ctx) }()

		<-comp.started

		errRun := a.Run(ctx)
		if !errors.Is(errRun, ErrAlreadyRunning) {
			t.Fatalf("second Run error = %v, want ErrAlreadyRunning", errRun)
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

// ctxComponent — компонент, наблюдающий отмену своего контекста.
type ctxComponent struct {
	fakeComponent
	canceled chan struct{}
	onRun    func()
}

// Run запускает горутину, ожидающую отмены контекста.
func (c *ctxComponent) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		close(c.canceled)
	}()

	if c.onRun != nil {
		c.onRun()
	}

	return c.fakeComponent.Run(ctx)
}

// TestRunCancelsComponentContext проверяет отмену контекста компонентов после Terminate.
func TestRunCancelsComponentContext(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	a := newTestApp(t)
	comp := &ctxComponent{
		fakeComponent: fakeComponent{name: "a", rec: rec, started: make(chan struct{})},
		canceled:      make(chan struct{}),
	}

	a.AddComponent(comp)

	done := make(chan error, 1)

	go func() { done <- a.Run(context.Background()) }()

	<-comp.started
	a.Terminate(errors.New("stop"))

	select {
	case <-comp.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("component context was not canceled")
	}

	<-done
}

// TestRunTerminateDuringStart проверяет откат при Terminate во время медленного запуска.
func TestRunTerminateDuringStart(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	a := newTestApp(t)
	errCause := errors.New("cause")
	slow := &ctxComponent{
		fakeComponent: fakeComponent{name: "slow", rec: rec},
		canceled:      make(chan struct{}),
	}

	slow.onRun = func() {
		a.Terminate(errCause)
		<-slow.canceled
	}

	a.AddComponent(&fakeComponent{name: "a", rec: rec}, slow, &fakeComponent{name: "c", rec: rec})

	errRun := a.Run(context.Background())
	if !errors.Is(errRun, errCause) {
		t.Fatalf("Run error = %v, want cause", errRun)
	}

	want := []string{"run a", "run slow", "stop slow", "stop a"}
	if got := rec.entries(); !slices.Equal(got, want) {
		t.Fatalf("log = %v, want %v", got, want)
	}
}

// TestRunCancelDuringStart проверяет, что отмена ctx во время запуска останавливает уже запущенные компоненты.
func TestRunCancelDuringStart(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	a := newTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	slow := &ctxComponent{
		fakeComponent: fakeComponent{name: "slow", rec: rec},
		canceled:      make(chan struct{}),
	}

	slow.onRun = func() {
		cancel()
		<-slow.canceled
	}

	a.AddComponent(&fakeComponent{name: "a", rec: rec}, slow, &fakeComponent{name: "c", rec: rec})

	errRun := a.Run(ctx)
	if errRun != nil {
		t.Fatalf("Run: %v", errRun)
	}

	want := []string{"run a", "run slow", "stop slow", "stop a"}
	if got := rec.entries(); !slices.Equal(got, want) {
		t.Fatalf("log = %v, want %v", got, want)
	}
}
