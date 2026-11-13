package bot

import (
	"testing"

	"go.uber.org/mock/gomock"
)

// TestReplyWithFullPanelStoreReportsFailure сообщает об отказе панели и завершает ожидание ответа.
func TestReplyWithFullPanelStoreReportsFailure(t *testing.T) {
	t.Parallel()

	b, deps := newTestBot(t)
	t.Cleanup(func() {
		errStop := b.Stop()
		if errStop != nil {
			t.Error(errStop)
		}
	})

	for range maxUserPanels {
		_, errPanel := b.preparePanel(100, [][]buttonSpec{{{Text: "Отмена", Cancel: true}}})
		if errPanel != nil {
			t.Fatal(errPanel)
		}
	}

	callGate(t, b, deps, 42, 100)
	deps.sender.EXPECT().Send(int64(100), "Не удалось подготовить сообщение, повторите команду").Return(nil).Times(1)
	deps.publisher.EXPECT().SetValue(gomock.Any(), "send", "Открыть?").Return(nil).Times(1)

	b.HandleSend([]byte(`{"reply_to":"42","text":"Открыть?","buttons":[[{"text":"Да","command":"gate","args":"open"}]]}`))

	deps.timers.mu.Lock()
	timerCount := len(deps.timers.stopped)
	timerStopped := timerCount == 1 && deps.timers.stopped[0]
	deps.timers.mu.Unlock()

	if !timerStopped {
		t.Fatalf("reply timer not stopped: count=%d", timerCount)
	}

	b.mu.Lock()
	answered := b.pending["42"].answered
	panelCount := len(b.panels)
	b.mu.Unlock()

	if !answered || panelCount != maxUserPanels {
		t.Fatalf("answered=%v panels=%d", answered, panelCount)
	}

	deps.timers.fire(0)
}
