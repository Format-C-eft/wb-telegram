package bot

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/Format-C-eft/wb-telegram/internal/bot/mock"
	"github.com/Format-C-eft/wb-telegram/internal/chart"
	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

const chartPayload = `{"id":"chart1","chart":{"title":"Температура","period_s":3600,"channels":[{"device":"boiler","control":"temperature","label":"Подача","unit":"C"}]}}`

// startChartBot запускает тестовое ядро с построителем графиков.
func startChartBot(t *testing.T, builder ChartBuilder) (*Bot, testDeps) {
	t.Helper()
	b, deps := newTestBot(t, WithChartBuilder(builder))
	deps.sender.EXPECT().SetCommands(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	deps.sender.EXPECT().Send(int64(100), "🤖 wb-telegram dev запущен").Return(nil)

	errRun := b.Run(context.Background())
	if errRun != nil {
		t.Fatal(errRun)
	}

	t.Cleanup(func() {
		if errStop := b.Stop(); errStop != nil {
			t.Error(errStop)
		}
	})

	return b, deps
}

// awaitSignal ожидает событие с ограничением времени.
func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("event timed out")
	}
}

// TestChartDoesNotBlockText не задерживает обычный ответ во время чтения истории.
func TestChartDoesNotBlockText(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	builder := mock.NewMockChartBuilder(ctrl)
	started := make(chan struct{})
	release := make(chan struct{})
	delivered := make(chan struct{})

	builder.EXPECT().Build(gomock.Any(), gomock.Any(), testNow).DoAndReturn(func(ctx context.Context, _ chart.Spec, _ time.Time) (chart.Result, error) {
		close(started)

		select {
		case <-release:
			return chart.Result{PNG: []byte("png"), Caption: "Температура"}, nil
		case <-ctx.Done():
			return chart.Result{}, chart.ErrCanceled
		}
	})
	b, deps := startChartBot(t, builder)
	deps.publisher.EXPECT().SetValue("send", "Температура").Return(nil)
	deps.publisher.EXPECT().SetValue("send", "Обычный текст").Return(nil)
	deps.sender.EXPECT().Send(int64(100), "Обычный текст").Return(nil)
	deps.sender.EXPECT().SendDelivery(int64(100), gomock.Any()).DoAndReturn(func(_ int64, d telegram.Delivery) error {
		if d.Kind != telegram.DeliveryPhoto || d.Caption != "Температура" {
			t.Errorf("delivery = %+v", d)
		}

		close(delivered)

		return nil
	})
	b.HandleSend([]byte(chartPayload))
	awaitSignal(t, started)
	b.HandleSend([]byte(`{"text":"Обычный текст"}`))
	close(release)
	awaitSignal(t, delivered)
}

// TestChartStopCancelsHistory завершает активное построение перед возвратом Stop.
func TestChartStopCancelsHistory(t *testing.T) {
	t.Parallel()
	builder := mock.NewMockChartBuilder(gomock.NewController(t))
	started := make(chan struct{})
	finished := make(chan struct{})

	builder.EXPECT().Build(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, _ chart.Spec, _ time.Time) (chart.Result, error) {
		close(started)
		<-ctx.Done()
		close(finished)

		return chart.Result{}, chart.ErrCanceled
	})
	b, deps := startChartBot(t, builder)
	deps.publisher.EXPECT().SetValue("send", "Температура").Return(nil)
	b.HandleSend([]byte(chartPayload))
	awaitSignal(t, started)

	if errStop := b.Stop(); errStop != nil {
		t.Fatal(errStop)
	}

	awaitSignal(t, finished)
	b.HandleSend([]byte(chartPayload))
}

// TestChartDeduplicatesRequest не строит один запрос повторно.
func TestChartDeduplicatesRequest(t *testing.T) {
	t.Parallel()
	builder := mock.NewMockChartBuilder(gomock.NewController(t))
	delivered := make(chan struct{})

	builder.EXPECT().Build(gomock.Any(), gomock.Any(), gomock.Any()).Return(chart.Result{PNG: []byte("png")}, nil)
	b, deps := startChartBot(t, builder)
	deps.publisher.EXPECT().SetValue("send", "Температура").Return(nil)
	deps.sender.EXPECT().SendDelivery(int64(100), gomock.Any()).DoAndReturn(func(_ int64, _ telegram.Delivery) error {
		close(delivered)

		return nil
	})
	b.HandleSend([]byte(chartPayload))
	awaitSignal(t, delivered)
	b.HandleSend([]byte(chartPayload))
}

// TestChartReplyClosesTimer завершает ожидание ответа до долгого построения.
func TestChartReplyClosesTimer(t *testing.T) {
	t.Parallel()
	builder := mock.NewMockChartBuilder(gomock.NewController(t))
	started := make(chan struct{})

	builder.EXPECT().Build(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, _ chart.Spec, _ time.Time) (chart.Result, error) {
		close(started)
		<-ctx.Done()

		return chart.Result{}, chart.ErrCanceled
	})
	b, deps := startChartBot(t, builder)
	callGate(b, deps, 10, 100)
	deps.publisher.EXPECT().SetValue("send", "Температура").Return(nil)
	b.HandleSend([]byte(`{"reply_to":"10","chart":{"title":"Температура","period_s":3600,"channels":[{"device":"boiler","control":"temperature","label":"Подача"}]}}`))
	awaitSignal(t, started)
	b.mu.Lock()
	answered := b.pending["10"].answered
	b.mu.Unlock()

	if !answered {
		t.Fatal("chart did not acknowledge reply")
	}

	deps.timers.fire(0)
	b.mu.Lock()
	timeoutNotified := b.pending["10"].timeoutNotified
	b.mu.Unlock()

	if timeoutNotified {
		t.Fatal("timeout won after chart admission")
	}
}

// TestStopWaitsForCallback ожидает завершения уже принятой обработки нажатия.
func TestStopWaitsForCallback(t *testing.T) {
	t.Parallel()
	b, deps := newTestBot(t)

	panel, errPanel := b.preparePanel(100, [][]buttonSpec{{{Text: "Отмена", Cancel: true}}})
	if errPanel != nil {
		t.Fatal(errPanel)
	}

	entered := make(chan struct{})

	deps.sender.EXPECT().AnswerCallback(gomock.Any(), "q", "Отменено", false).DoAndReturn(func(ctx context.Context, _ string, _ string, _ bool) error {
		close(entered)
		<-ctx.Done()

		return nil
	})

	var workers sync.WaitGroup

	workers.Go(func() {
		b.HandleCallback(context.Background(), telegram.Callback{QueryID: "q", ChatType: "private", ChatID: 100, FromID: 100, Accessible: true, Data: panel.keyboard[0][0].Data})
	})

	awaitSignal(t, entered)

	if errStop := b.Stop(); errStop != nil {
		t.Fatal(errStop)
	}

	workers.Wait()

	if len(b.panels) != 0 || len(b.buttons) != 0 {
		t.Fatal("stop retained keyboard state")
	}
}

// TestChartLateReply сохраняет поздний график после логически сработавшего таймера.
func TestChartLateReply(t *testing.T) {
	t.Parallel()
	builder := mock.NewMockChartBuilder(gomock.NewController(t))
	builder.EXPECT().Build(gomock.Any(), gomock.Any(), gomock.Any()).Return(chart.Result{PNG: []byte("png")}, nil)
	b, deps := startChartBot(t, builder)
	callGate(b, deps, 10, 100)

	timeoutEntered := make(chan struct{})
	releaseTimeout := make(chan struct{})
	timeoutDone := make(chan struct{})
	delivered := make(chan struct{})

	deps.sender.EXPECT().Send(int64(100), "Команда /gate отправлена, ответа от правил нет").DoAndReturn(func(_ int64, _ string) error {
		close(timeoutEntered)

		select {
		case <-releaseTimeout:
		case <-b.operationCtx.Done():
		}

		return nil
	})
	deps.publisher.EXPECT().SetValue("send", "Температура").Return(nil)
	deps.sender.EXPECT().SendDelivery(int64(100), gomock.Any()).DoAndReturn(func(_ int64, _ telegram.Delivery) error {
		close(delivered)

		return nil
	})

	go func() {
		defer close(timeoutDone)

		deps.timers.fire(0)
	}()

	awaitSignal(t, timeoutEntered)
	b.HandleSend([]byte(`{"reply_to":"10","chart":{"title":"Температура","period_s":3600,"channels":[{"device":"boiler","control":"temperature","label":"Подача"}]}}`))
	awaitSignal(t, delivered)
	close(releaseTimeout)
	awaitSignal(t, timeoutDone)
}

// TestChartFailureReportsText возвращает понятное сообщение об отказе графика.
func TestChartFailureReportsText(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		failure error
		text    string
	}{
		{"empty", chart.ErrNoData, "За выбранный период данных нет"},
		{"nonnumeric", chart.ErrNonNumeric, "Канал не содержит числовых данных"},
		{"overflow", chart.ErrTooLarge, "Слишком много данных: уменьшите период графика"},
		{"history", chart.ErrHistory, "Не удалось получить историю и построить график"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			builder := mock.NewMockChartBuilder(gomock.NewController(t))
			builder.EXPECT().Build(gomock.Any(), gomock.Any(), gomock.Any()).Return(chart.Result{}, tc.failure)
			b, deps := startChartBot(t, builder)
			done := make(chan struct{})

			deps.publisher.EXPECT().SetValue("send", "Температура").Return(nil)
			deps.sender.EXPECT().Send(int64(100), tc.text).DoAndReturn(func(_ int64, _ string) error {
				close(done)

				return nil
			})
			b.HandleSend([]byte(chartPayload))
			awaitSignal(t, done)
		})
	}
}
