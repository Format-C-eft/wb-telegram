package bot

import (
	"fmt"
	"strings"
	"time"

	"github.com/Format-C-eft/wb-telegram/internal/config"
)

// statusCommand — встроенная команда сводки о боте; доступна пользователям с системными уведомлениями.
const statusCommand = "status"

// statusDescription — описание /status в меню и справке.
const statusDescription = "Состояние бота"

// outageWindow — окно подсчёта обрывов связи с Telegram в /status.
const outageWindow = 24 * time.Hour

// lastCallInfo — последний опубликованный вызов команды.
type lastCallInfo struct {
	id      string
	command string
	user    string
	at      time.Time
	latency time.Duration
	replied bool
}

// serviceStats — состояние и счётчики бота с момента запуска для /status и служебных сообщений.
type serviceStats struct {
	startedAt time.Time

	telegramDownSince time.Time
	telegramReason    string
	telegramOutages   []time.Time
	telegramLastStart time.Time
	telegramLastFor   time.Duration

	mqttDownSince   time.Time
	mqttReconnects  int
	mqttLastConnect time.Time

	calls      int
	unanswered int
	stale      int
	lastCall   *lastCallInfo
}

// ReportTelegram отражает смену состояния связи с Bot API; после восстановления присылает
// получателям системных уведомлений, сколько не было связи и почему. Сообщение о потере
// не отправляется: Telegram в этот момент недоступен.
func (b *Bot) ReportTelegram(ok bool, reason string) {
	now := b.options.now()

	b.statsMu.Lock()

	if !ok {
		if b.stats.telegramDownSince.IsZero() {
			b.stats.telegramDownSince = now
			b.stats.telegramReason = reason
		}

		b.statsMu.Unlock()

		return
	}

	since := b.stats.telegramDownSince
	cause := b.stats.telegramReason

	if !since.IsZero() {
		b.stats.telegramDownSince = time.Time{}
		b.stats.telegramOutages = append(pruneOutages(b.stats.telegramOutages, now), since)
		b.stats.telegramLastStart = since
		b.stats.telegramLastFor = now.Sub(since)
	}

	b.statsMu.Unlock()

	if since.IsZero() {
		return
	}

	b.notifyService(fmt.Sprintf("✅ Связь с Telegram восстановлена. Не было %s (с %s), причина: %s",
		formatDuration(now.Sub(since)), b.clock(since), describeReason(cause)))
}

// ReportMQTT отражает связь с MQTT-брокером контроллера: сообщает о потере сразу (Telegram при
// этом работает) и о восстановлении — с длительностью.
func (b *Bot) ReportMQTT(ok bool) {
	now := b.options.now()

	b.statsMu.Lock()

	since := b.stats.mqttDownSince

	switch {
	case !ok && since.IsZero():
		b.stats.mqttDownSince = now
	case ok && !since.IsZero():
		b.stats.mqttDownSince = time.Time{}
		b.stats.mqttReconnects++
		b.stats.mqttLastConnect = now
	}

	b.statsMu.Unlock()

	switch {
	case !ok && since.IsZero():
		b.notifyService("⚠️ Нет связи с MQTT-брокером контроллера: команды не выполняются")
	case ok && !since.IsZero():
		b.notifyService(fmt.Sprintf("✅ Связь с MQTT-брокером восстановлена. Не было %s (с %s)",
			formatDuration(now.Sub(since)), b.clock(since)))
	}
}

// notifyService отправляет служебное сообщение получателям системных уведомлений.
func (b *Bot) notifyService(text string) {
	if b.isStopped() {
		return
	}

	for _, user := range b.cfg.NotificationUsers() {
		b.deliver(user.ChatID, text)
	}
}

// countStale учитывает устаревшую команду.
func (b *Bot) countStale() {
	b.statsMu.Lock()
	b.stats.stale++
	b.statsMu.Unlock()
}

// countCall учитывает опубликованный вызов и запоминает его как последний.
func (b *Bot) countCall(id, command, user string, at time.Time) {
	b.statsMu.Lock()

	b.stats.calls++
	b.stats.lastCall = &lastCallInfo{id: id, command: command, user: user, at: at}

	b.statsMu.Unlock()
}

// countUnanswered учитывает вызов, на который правила не ответили вовремя.
func (b *Bot) countUnanswered() {
	b.statsMu.Lock()
	b.stats.unanswered++
	b.statsMu.Unlock()
}

// countReply запоминает время первого ответа правила на последний вызов.
func (b *Bot) countReply(id string) {
	now := b.options.now()

	b.statsMu.Lock()

	last := b.stats.lastCall
	if last != nil && last.id == id && !last.replied {
		last.replied = true
		last.latency = now.Sub(last.at)
	}

	b.statsMu.Unlock()
}

// statusText собирает ответ на /status.
func (b *Bot) statusText() string {
	now := b.options.now()
	telegramStats := b.sender.Stats()

	b.statsMu.Lock()

	stats := b.stats
	stats.telegramOutages = pruneOutages(b.stats.telegramOutages, now)

	if b.stats.lastCall != nil {
		lastCopy := *b.stats.lastCall
		stats.lastCall = &lastCopy
	}

	b.statsMu.Unlock()

	lines := []string{
		"\U0001F916 wb-telegram " + b.options.version,
		fmt.Sprintf("Работает %s (с %s)", formatDuration(now.Sub(stats.startedAt)), b.dateTime(stats.startedAt)),
		"",
		"Telegram: " + yesNo(telegramStats.Connected && stats.telegramDownSince.IsZero(), "связь есть", "нет связи"),
		"  " + b.outagesLine(stats),
		"MQTT: " + yesNo(b.publisher.Connected(), "подключено", "нет связи"),
		"  " + b.reconnectsLine(stats),
		"",
		fmt.Sprintf("Сообщения: отправлено %d, не доставлено %d, в очереди %d",
			telegramStats.Sent, telegramStats.Dropped, telegramStats.Queued),
		fmt.Sprintf("Команды: вызовов %d, без ответа правил %d, устаревших %d", stats.calls, stats.unanswered, stats.stale),
	}

	if stats.lastCall != nil {
		lines = append(lines, "  "+b.lastCallLine(*stats.lastCall))
	}

	lines = append(lines, "", b.settingsLine())

	return strings.Join(lines, "\n")
}

// outagesLine описывает обрывы связи с Telegram.
func (b *Bot) outagesLine(stats serviceStats) string {
	line := "обрывов за сутки: " + fmt.Sprint(len(stats.telegramOutages))

	if !stats.telegramLastStart.IsZero() {
		line += fmt.Sprintf(", последний %s, длился %s", b.dateTime(stats.telegramLastStart), formatDuration(stats.telegramLastFor))
	}

	return line
}

// reconnectsLine описывает переподключения к MQTT.
func (b *Bot) reconnectsLine(stats serviceStats) string {
	line := "переподключений: " + fmt.Sprint(stats.mqttReconnects)

	if !stats.mqttLastConnect.IsZero() {
		line += ", последнее " + b.dateTime(stats.mqttLastConnect)
	}

	return line
}

// lastCallLine описывает последний вызов команды.
func (b *Bot) lastCallLine(last lastCallInfo) string {
	line := fmt.Sprintf("последняя: /%s — %s, %s", last.command, last.user, b.dateTime(last.at))

	if last.replied {
		return line + ", ответ за " + strings.Replace(fmt.Sprintf("%.1f", last.latency.Seconds()), ".", ",", 1) + " с"
	}

	return line + ", ответа не было"
}

// settingsLine описывает действующие настройки.
func (b *Bot) settingsLine() string {
	return fmt.Sprintf("Настройки: %s, %s; таймауты: подключение %d с, опрос %d с, ответ правила %d с",
		plural(len(b.cfg.Users), "пользователь", "пользователя", "пользователей"),
		plural(len(b.cfg.Commands), "команда", "команды", "команд"),
		b.cfg.ConnectTimeoutS, b.cfg.PollTimeoutS, b.cfg.ReplyTimeoutS)
}

// statusAllowed сообщает, доступна ли пользователю команда /status.
func statusAllowed(user config.User) bool {
	return user.Notifications
}

// clock форматирует время суток в часовом поясе бота.
func (b *Bot) clock(t time.Time) string {
	return t.In(b.options.location).Format("15:04:05")
}

// dateTime форматирует дату и время в часовом поясе бота; для сегодняшнего дня — только время.
func (b *Bot) dateTime(t time.Time) string {
	local := t.In(b.options.location)
	now := b.options.now().In(b.options.location)

	if local.YearDay() == now.YearDay() && local.Year() == now.Year() {
		return local.Format("15:04")
	}

	return local.Format("02.01 15:04")
}

// pruneOutages убирает обрывы старше окна outageWindow.
func pruneOutages(outages []time.Time, now time.Time) []time.Time {
	kept := make([]time.Time, 0, len(outages))

	for _, start := range outages {
		if now.Sub(start) <= outageWindow {
			kept = append(kept, start)
		}
	}

	return kept
}

// describeReason переводит ошибку связи в короткую понятную причину.
func describeReason(raw string) string {
	lower := strings.ToLower(raw)

	switch {
	case raw == "":
		return "неизвестна"
	case strings.Contains(lower, "tls handshake timeout"):
		return "долгое TLS-рукопожатие"
	case strings.Contains(lower, "deadline exceeded") || strings.Contains(lower, "timeout"):
		return "нет ответа от Telegram"
	case strings.Contains(lower, "connection lost") || strings.Contains(lower, "eof") || strings.Contains(lower, "reset"):
		return "соединение оборвалось"
	case strings.Contains(lower, "no such host") || strings.Contains(lower, "dns"):
		return "не найден адрес api.telegram.org (DNS)"
	case strings.Contains(lower, "refused") || strings.Contains(lower, "unreachable"):
		return "нет маршрута до Telegram"
	}

	if index := strings.LastIndex(raw, ": "); index >= 0 {
		return raw[index+2:]
	}

	return raw
}

// formatDuration форматирует длительность по-человечески: «1 мин 17 с», «2 ч 5 мин», «3 д 4 ч».
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)

	days := int(d / (24 * time.Hour))
	hours := int(d % (24 * time.Hour) / time.Hour)
	minutes := int(d % time.Hour / time.Minute)
	seconds := int(d % time.Minute / time.Second)

	switch {
	case days > 0:
		return fmt.Sprintf("%d д %d ч", days, hours)
	case hours > 0:
		return fmt.Sprintf("%d ч %d мин", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%d мин %d с", minutes, seconds)
	}

	return fmt.Sprintf("%d с", seconds)
}

// plural возвращает число с существительным в нужной форме: 1 пользователь, 2 пользователя, 5 пользователей.
func plural(n int, one, few, many string) string {
	mod10, mod100 := n%10, n%100

	switch {
	case mod10 == 1 && mod100 != 11:
		return fmt.Sprintf("%d %s", n, one)
	case mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14):
		return fmt.Sprintf("%d %s", n, few)
	}

	return fmt.Sprintf("%d %s", n, many)
}

// yesNo выбирает значок и текст состояния.
func yesNo(ok bool, yes, no string) string {
	if ok {
		return "✅ " + yes
	}

	return "❌ " + no
}
