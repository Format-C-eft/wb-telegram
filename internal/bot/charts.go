package bot

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/chart"
	"github.com/Format-C-eft/wb-telegram/internal/telegram"
)

// chartJob хранит ограниченное задание построения графика.
type chartJob struct {
	id         string
	spec       chart.Spec
	end        time.Time
	recipients []int64
	ephemeral  ephemeralReply
}

// chartSeenEntry хранит состояние дедупликации задания.
type chartSeenEntry struct {
	at     time.Time
	active bool
}

// handleChart принимает задание и сохраняет описание в контроле send.
func (b *Bot) handleChart(req sendRequest) {
	if req.Ephemeral {
		target := b.takeEphemeralReply(req.ReplyTo)
		if target.queryID != "" {
			b.handleEphemeralChart(req, target)
		}

		return
	}

	recipients := b.chartRecipients(req)
	if len(recipients) == 0 {
		return
	}

	accepted, errAdmit := b.admitChart(req, recipients)
	if errAdmit != nil {
		for _, chatID := range recipients {
			b.deliver(chatID, "График пока недоступен: обработка занята или история не подключена")
		}

		return
	}

	if accepted {
		b.publishSummary(req.Chart.Title)
	}
}

// chartRecipients находит получателей без побочного завершения ожидания ответа.
func (b *Bot) chartRecipients(req sendRequest) []int64 {
	if req.ReplyTo == "" {
		return b.recipients(req)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	call := b.pendingReply(req.ReplyTo)
	if call == nil || call.source.widget != nil {
		return nil
	}

	return []int64{call.chatID}
}

// admitChart резервирует адресатов и атомарно завершает ожидание ответа правила.
func (b *Bot) admitChart(req sendRequest, recipients []int64) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stopped {
		return false, ErrInvalidState
	}

	if req.ReplyTo != "" {
		call := b.pendingReply(req.ReplyTo)
		if call == nil || call.source.widget != nil {
			return false, ErrInvalidState
		}

		b.completeReply(req.ReplyTo, call)
	}

	return b.enqueueChartLocked(req, recipients, ephemeralReply{})
}

// handleEphemeralChart принимает временный график только при свободном построителе.
func (b *Bot) handleEphemeralChart(req sendRequest, target ephemeralReply) {
	b.mu.Lock()

	var errAdmit error = ErrInvalidState

	if !b.stopped && len(b.chartBusy) == 0 {
		_, errAdmit = b.enqueueChartLocked(req, []int64{target.chatID}, target)
	}

	b.mu.Unlock()

	if errAdmit != nil {
		b.deliverEphemeral(target, telegram.Delivery{Kind: telegram.DeliveryText, Text: "График пока недоступен: обработка занята или история не подключена"})
	}
}

// enqueueChartLocked резервирует получателей и добавляет задание.
func (b *Bot) enqueueChartLocked(req sendRequest, recipients []int64, target ephemeralReply) (bool, error) {
	var id string
	if target.queryID == "" {
		id = req.deduplicationID()
	}

	b.pruneChartSeenLocked()

	if id != "" {
		if _, duplicate := b.chartSeen[id]; duplicate {
			return false, nil
		}
	}

	if b.options.chartBuilder == nil || b.cancel == nil || len(b.chartJobs) >= cap(b.chartJobs) {
		return false, ErrInvalidState
	}

	for _, chatID := range recipients {
		if b.chartBusy[chatID] {
			return false, ErrInvalidState
		}
	}

	for _, chatID := range recipients {
		b.chartBusy[chatID] = true
	}

	if id != "" {
		b.chartSeen[id] = chartSeenEntry{at: b.options.now(), active: true}
	}

	b.chartJobs <- chartJob{id: id, spec: *req.Chart, end: b.options.now(), recipients: append([]int64(nil), recipients...), ephemeral: target}

	return true, nil
}

// pruneChartSeenLocked ограничивает время и объём дедупликации под mutex ядра.
func (b *Bot) pruneChartSeenLocked() {
	now := b.options.now()
	for id, entry := range b.chartSeen {
		if !entry.active && now.Sub(entry.at) >= 5*time.Minute {
			delete(b.chartSeen, id)
		}
	}

	if len(b.chartSeen) < 512 {
		return
	}

	var (
		oldestID string
		oldest   time.Time
	)
	for id, entry := range b.chartSeen {
		if !entry.active && (oldestID == "" || entry.at.Before(oldest)) {
			oldestID = id
			oldest = entry.at
		}
	}

	if oldestID != "" {
		delete(b.chartSeen, oldestID)
	}
}

// chartLoop последовательно обрабатывает задания до остановки ядра.
func (b *Bot) chartLoop() {
	defer b.chartWorkers.Done()

	for {
		select {
		case <-b.operationCtx.Done():
			b.drainCharts()

			return
		case job := <-b.chartJobs:
			if b.operationCtx.Err() == nil {
				b.buildChart(job)
			}

			b.releaseChart(job)
		}
	}
}

// drainCharts освобождает не начатые задания после остановки.
func (b *Bot) drainCharts() {
	for {
		select {
		case job := <-b.chartJobs:
			b.releaseChart(job)
		default:
			return
		}
	}
}

// releaseChart освобождает резервы получателей завершённого задания.
func (b *Bot) releaseChart(job chartJob) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, chatID := range job.recipients {
		delete(b.chartBusy, chatID)
	}

	if job.id != "" {
		b.chartSeen[job.id] = chartSeenEntry{at: b.options.now()}
	}
}

// buildChart строит изображение и передаёт его на отправку.
func (b *Bot) buildChart(job chartJob) {
	if b.operationCtx.Err() != nil {
		return
	}

	timeout := 15 * time.Second
	if job.ephemeral.queryID != "" {
		timeout = job.ephemeral.deadline.Sub(b.options.now()) - shortSendBudget
		if timeout <= 0 {
			b.deliverEphemeral(job.ephemeral, telegram.Delivery{Kind: telegram.DeliveryText, Text: "Не хватило времени построить график. Повторите запрос."})

			return
		}
	}

	ctx, cancel := context.WithTimeout(b.operationCtx, timeout)
	defer cancel()

	result, errBuild := b.options.chartBuilder.Build(ctx, job.spec, job.end)
	if b.operationCtx.Err() != nil {
		return
	}

	if errBuild != nil {
		text := chartFailureText(errBuild)
		if job.ephemeral.queryID != "" {
			b.deliverEphemeral(job.ephemeral, telegram.Delivery{Kind: telegram.DeliveryText, Text: text})

			return
		}

		for _, chatID := range job.recipients {
			b.deliver(chatID, text)
		}

		return
	}

	delivery := telegram.Delivery{Kind: telegram.DeliveryPhoto, PNG: result.PNG, Caption: result.Caption}
	if job.ephemeral.queryID != "" {
		b.deliverEphemeral(job.ephemeral, delivery)

		return
	}

	for _, chatID := range job.recipients {
		if b.operationCtx.Err() != nil {
			return
		}

		errSend := b.sender.SendDelivery(chatID, delivery)
		if errSend != nil && !errors.Is(errSend, telegram.ErrQueueFull) {
			slog.Warn("Telegram: график не принят в очередь", "chat_id", chatID, "err", errSend)
		}
	}
}

// chartFailureText возвращает сообщение об отказе построителя.
func chartFailureText(errBuild error) string {
	switch {
	case errors.Is(errBuild, chart.ErrNoData):
		return "За выбранный период данных нет"
	case errors.Is(errBuild, chart.ErrNonNumeric):
		return "Канал не содержит числовых данных"
	case errors.Is(errBuild, chart.ErrTooLarge):
		return "Слишком много данных: уменьшите период графика"
	default:
		return "Не удалось получить историю и построить график"
	}
}

// deduplicationID извлекает строковый id без изменения старого текстового контракта.
func (req sendRequest) deduplicationID() string {
	if len(req.ID) == 0 {
		return ""
	}

	var id string

	errID := json.Unmarshal(req.ID, &id)
	if errID != nil {
		return ""
	}

	return id
}
