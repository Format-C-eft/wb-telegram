package app

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// App управляет запуском и остановкой компонентов сервиса.
type App struct {
	mu         sync.Mutex
	components []Component
	closers    []func() error
	running    bool
	terminate  chan error
	options    options
}

// New создаёт App с переданными опциями.
func New(opts ...Option) (*App, error) {
	o := defaultOptions()

	for _, opt := range opts {
		opt(&o)
	}

	errValidate := o.Validate()
	if errValidate != nil {
		return nil, errValidate
	}

	return &App{terminate: make(chan error, 1), options: o}, nil
}

// AddComponent регистрирует компоненты; запускаются в порядке регистрации.
func (a *App) AddComponent(components ...Component) {
	a.mu.Lock()
	a.components = append(a.components, components...)
	a.mu.Unlock()
}

// AddCloser регистрирует функции очистки; вызываются после остановки компонентов.
func (a *App) AddCloser(closers ...func() error) {
	a.mu.Lock()
	a.closers = append(a.closers, closers...)
	a.mu.Unlock()
}

// Terminate просит App завершиться с причиной cause; повторные вызовы игнорируются.
func (a *App) Terminate(cause error) {
	select {
	case a.terminate <- cause:
	default:
	}
}

// Run запускает компоненты и блокирует до отмены ctx, сигнала остановки или Terminate.
// Компоненты получают контекст, который отменяется при любом из этих событий.
func (a *App) Run(ctx context.Context) error {
	a.mu.Lock()

	if a.running {
		a.mu.Unlock()

		return ErrAlreadyRunning
	}

	a.running = true
	components := slices.Clone(a.components)
	closers := slices.Clone(a.closers)

	a.mu.Unlock()

	defer a.finish()

	signalCtx, stopSignals := a.options.signalContext()
	defer stopSignals()

	runCtx, cancel := context.WithCancel(signalCtx)
	defer cancel()

	stopAfter := context.AfterFunc(ctx, cancel)
	defer stopAfter()

	causeCh := make(chan error, 1)
	watcherDone := make(chan struct{})

	go func() {
		defer close(watcherDone)

		select {
		case cause := <-a.terminate:
			causeCh <- cause

			cancel()
		case <-runCtx.Done():
		}
	}()

	started, errStart := startComponents(runCtx, components)
	if errStart != nil {
		errShutdown := a.shutdown(started, closers)

		cancel()
		<-watcherDone
		a.drainTerminate()

		return errors.Join(errStart, errShutdown)
	}

	<-runCtx.Done()
	<-watcherDone

	var cause error

	select {
	case cause = <-causeCh:
	default:
	}

	errShutdown := a.shutdown(started, closers)

	cancel()
	a.drainTerminate()

	return errors.Join(cause, errShutdown)
}

// drainTerminate сбрасывает неиспользованную причину остановки, чтобы следующий Run не завершился сразу.
func (a *App) drainTerminate() {
	select {
	case <-a.terminate:
	default:
	}
}

// shutdown останавливает компоненты и вызывает closers; таймаутом ограничен только Shutdown(ctx).
func (a *App) shutdown(started []Component, closers []func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), a.options.shutdownTimeout)
	defer cancel()

	errStop := stopComponents(ctx, started)
	errClose := runClosers(closers)

	return errors.Join(errStop, errClose)
}

// finish снимает признак работы App.
func (a *App) finish() {
	a.mu.Lock()
	a.running = false
	a.mu.Unlock()
}
