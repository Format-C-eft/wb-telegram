package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// Component — компонент, жизненным циклом которого управляет App.
// Run не блокирует: запускает фоновую работу и возвращается.
type Component interface {
	Run(ctx context.Context) error
	Stop() error
}

// GracefulComponent — компонент с остановкой, учитывающей context.
type GracefulComponent interface {
	Component
	Shutdown(ctx context.Context) error
}

// startComponents запускает компоненты в порядке регистрации и возвращает успешно запущенные.
// Если ctx отменён во время запуска, оставшиеся компоненты не запускаются.
func startComponents(ctx context.Context, components []Component) ([]Component, error) {
	started := make([]Component, 0, len(components))

	for index, component := range components {
		if ctx.Err() != nil {
			return started, nil
		}

		if component == nil {
			return started, &Error{kind: kindNilComponent, Op: "start", Message: fmt.Sprintf("component %d is nil", index)}
		}

		errRun := component.Run(ctx)
		if errRun != nil {
			return started, fmt.Errorf("start component %d: %w", index, errRun)
		}

		started = append(started, component)
	}

	return started, nil
}

// stopComponents останавливает компоненты в обратном порядке и собирает ошибки.
func stopComponents(ctx context.Context, components []Component) error {
	var errs []error

	for index, component := range slices.Backward(components) {
		var errStop error

		if graceful, ok := component.(GracefulComponent); ok {
			errStop = graceful.Shutdown(ctx)
		} else {
			errStop = component.Stop()
		}

		if errStop != nil {
			errs = append(errs, fmt.Errorf("stop component %d: %w", index, errStop))
		}
	}

	return errors.Join(errs...)
}

// runClosers вызывает closers в обратном порядке регистрации.
func runClosers(closers []func() error) error {
	var errs []error

	for index, closer := range slices.Backward(closers) {
		errClose := closer()
		if errClose != nil {
			errs = append(errs, fmt.Errorf("closer %d: %w", index, errClose))
		}
	}

	return errors.Join(errs...)
}
