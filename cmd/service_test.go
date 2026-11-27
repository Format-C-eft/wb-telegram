package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/cmd/mock"
	"github.com/Format-C-eft/wb-telegram/internal/config"
	"github.com/Format-C-eft/wb-telegram/internal/telegram"
	"github.com/Format-C-eft/wb-telegram/internal/wbdevice"
)

// TestRunServiceExitCodes проверяет решения запуска без внешних подключений.
func TestRunServiceExitCodes(t *testing.T) {
	t.Parallel()

	errRead := errors.New("read failed")
	tests := []struct {
		name      string
		cfg       *config.Config
		loadError error
		tokenErr  error
		wantCode  int
	}{
		{name: "disabled", cfg: &config.Config{}, wantCode: 0},
		{name: "no token", cfg: &config.Config{Enabled: true}, tokenErr: config.ErrNoToken, wantCode: 0},
		{name: "broken config", loadError: errRead, wantCode: 1},
		{name: "invalid config", loadError: config.ErrInvalid, wantCode: 1},
		{name: "token unreadable", cfg: &config.Config{Enabled: true}, tokenErr: errRead, wantCode: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backend := mock.NewMockServiceBackend(gomock.NewController(t))
			backend.EXPECT().LoadConfig("config").Return(tt.cfg, tt.loadError)

			if tt.loadError == nil {
				if tt.cfg.Enabled {
					backend.EXPECT().ReadToken("token").Return("", tt.tokenErr)
				}

				if !tt.cfg.Enabled || errors.Is(tt.tokenErr, config.ErrNoToken) {
					backend.EXPECT().PurgeDevice("tcp://localhost:1883")
				}
			}

			code := runServiceWithBackend(context.Background(), serviceSettings{
				configPath: "config", tokenPath: "token", broker: "tcp://localhost:1883",
			}, backend)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tt.wantCode)
			}
		})
	}
}

// TestRunServiceEnabled проверяет передачу настроек и кода завершения приложению.
func TestRunServiceEnabled(t *testing.T) {
	t.Parallel()

	backend := mock.NewMockServiceBackend(gomock.NewController(t))
	cfg := &config.Config{Enabled: true, Debug: true}
	ctx := t.Context()

	level := new(slog.LevelVar)
	settings := serviceSettings{configPath: "config", tokenPath: "token", broker: "tcp://localhost:1883", apiURL: "https://api.telegram.org", logLevel: level}

	gomock.InOrder(
		backend.EXPECT().LoadConfig("config").Return(cfg, nil),
		backend.EXPECT().ReadToken("token").Return("123:example", nil),
		backend.EXPECT().RunBot(ctx, cfg, "123:example", settings.broker, settings.apiURL).Return(1),
	)

	code := runServiceWithBackend(ctx, settings, backend)
	if code != 1 || level.Level() != slog.LevelDebug {
		t.Fatalf("code=%d log level=%v", code, level.Level())
	}
}

// TestRunServiceDefaultBroker проверяет выбор штатного адреса брокера.
func TestRunServiceDefaultBroker(t *testing.T) {
	t.Parallel()

	backend := mock.NewMockServiceBackend(gomock.NewController(t))
	backend.EXPECT().LoadConfig("config").Return(&config.Config{}, nil)
	backend.EXPECT().PurgeDevice(wbdevice.DefaultBroker(wbdevice.MosquittoSocket))

	code := runServiceWithBackend(context.Background(), serviceSettings{configPath: "config"}, backend)
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
}

// TestRunApplication проверяет очистку устройства и код завершения приложения.
func TestRunApplication(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		errRun    error
		wantCode  int
		wantPurge bool
	}{
		{name: "stopped", wantCode: 0},
		{name: "unauthorized", errRun: telegram.ErrUnauthorized, wantCode: 0, wantPurge: true},
		{name: "wrapped unauthorized", errRun: errors.Join(errors.New("stop"), telegram.ErrUnauthorized), wantCode: 0, wantPurge: true},
		{name: "failed", errRun: errors.New("start failed"), wantCode: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			controller := gomock.NewController(t)
			runner := mock.NewMockApplicationRunner(controller)
			purger := mock.NewMockDevicePurger(controller)
			ctx := context.Background()
			runner.EXPECT().Run(ctx).Return(tt.errRun)

			if tt.wantPurge {
				purger.EXPECT().PurgeDevice("tcp://localhost:1883")
			}

			code := runApplication(ctx, runner, "tcp://localhost:1883", purger)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tt.wantCode)
			}
		})
	}
}

// TestRunApplicationCancellation проверяет передачу отменённого контекста приложению.
func TestRunApplicationCancellation(t *testing.T) {
	t.Parallel()

	controller := gomock.NewController(t)
	runner := mock.NewMockApplicationRunner(controller)
	purger := mock.NewMockDevicePurger(controller)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	runner.EXPECT().Run(ctx).DoAndReturn(func(runCtx context.Context) error {
		if !errors.Is(runCtx.Err(), context.Canceled) {
			t.Fatal("application did not receive cancellation")
		}

		return nil
	})

	code := runApplication(ctx, runner, "tcp://localhost:1883", purger)
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
}
