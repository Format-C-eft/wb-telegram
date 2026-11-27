package wbdevice

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/wbdevice/mock"
)

// TestDeviceStaleSetupKeepsShutdownFlag проверяет отсутствие пустого флага после остановки.
func TestDeviceStaleSetupKeepsShutdownFlag(t *testing.T) {
	t.Parallel()

	dev, client, _, controller := newMockDevice(t)
	stale := dev.nextGeneration()

	client.EXPECT().IsConnectionOpen().Return(true)
	publish := expectPublish(client, controller, deviceErrorTopic(dev.id), ErrorFlagRead)
	client.EXPECT().Disconnect(uint(250)).After(publish)

	if errShutdown := dev.Shutdown(context.Background()); errShutdown != nil {
		t.Fatal(errShutdown)
	}

	if errStale := dev.publishHealthy(stale); errStale != nil {
		t.Fatal(errStale)
	}

	if dev.Connected() {
		t.Fatal("device ready after shutdown")
	}

	if errValue := dev.SetValue(t.Context(), "send", "x"); !errors.Is(errValue, ErrNotConnected) {
		t.Fatalf("SetValue after shutdown = %v", errValue)
	}
}

// TestDeviceShutdownExpiredContext проверяет минимальное ожидание PUBACK флага r.
func TestDeviceShutdownExpiredContext(t *testing.T) {
	t.Parallel()

	dev, client, _, controller := newMockDevice(t, WithTimeout(2*time.Second))
	client.EXPECT().IsConnectionOpen().Return(true)

	token := mock.NewMockToken(controller)
	publish := client.EXPECT().Publish(deviceErrorTopic(dev.id), byte(1), true, ErrorFlagRead).Return(token)
	token.EXPECT().WaitTimeout(shutdownPublishFloor).Return(true)
	token.EXPECT().Error().Return(nil)
	client.EXPECT().Disconnect(uint(250)).After(publish)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	errShutdown := dev.Shutdown(ctx)
	if errShutdown != nil {
		t.Fatal(errShutdown)
	}
}

// TestDeviceShutdownWaitsForHandler проверяет ожидание активного обработчика после отключения.
func TestDeviceShutdownWaitsForHandler(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	dev, client, _, controller := newMockDevice(t, WithWriteHandler(func(string, []byte) { close(entered); <-release }))

	dev.workers.Add(1)
	go dev.dispatchWrites()

	dev.handleWrite(client, mockMessage(controller, controlOnTopic(dev.id, "send"), []byte("x"), false))
	<-entered
	client.EXPECT().IsConnectionOpen().Return(true)

	token := mock.NewMockToken(controller)
	publish := client.EXPECT().Publish(deviceErrorTopic(dev.id), byte(1), true, ErrorFlagRead).Return(token)
	ack := token.EXPECT().WaitTimeout(gomock.Any()).Return(true).After(publish)
	result := token.EXPECT().Error().Return(nil).After(ack)
	disconnected := make(chan struct{})

	client.EXPECT().Disconnect(uint(250)).Do(func(uint) { close(disconnected) }).After(result)

	done := make(chan error, 1)
	go func() { done <- dev.Shutdown(context.Background()) }()

	<-disconnected

	select {
	case errEarly := <-done:
		close(release)
		t.Fatalf("Shutdown returned before handler completed: %v", errEarly)
	default:
	}

	close(release)

	select {
	case errShutdown := <-done:
		if errShutdown != nil {
			t.Fatal(errShutdown)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after handler completed")
	}
}

// TestDeviceShutdownPublishesBeforeDisconnect проверяет отключение после таймаута PUBACK.
func TestDeviceShutdownPublishesBeforeDisconnect(t *testing.T) {
	t.Parallel()

	dev, client, _, controller := newMockDevice(t)
	client.EXPECT().IsConnectionOpen().Return(true)

	token := mock.NewMockToken(controller)
	publish := client.EXPECT().Publish(deviceErrorTopic(dev.id), byte(1), true, ErrorFlagRead).Return(token)
	ack := token.EXPECT().WaitTimeout(gomock.Any()).Return(false).After(publish)
	client.EXPECT().Disconnect(uint(250)).After(ack)

	errShutdown := dev.Shutdown(context.Background())
	if !errors.Is(errShutdown, ErrTimeout) {
		t.Fatalf("Shutdown = %v", errShutdown)
	}
}
