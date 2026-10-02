package bot

import (
	"bytes"
	"testing"
)

// cesuFire — эмодзи 🔥 (U+1F525) так, как его отдаёт wb-rules: суррогатная пара D83D DD25 в CESU-8.
var cesuFire = []byte{0xED, 0xA0, 0xBD, 0xED, 0xB4, 0xA5}

// TestFixCESU8 проверяет сборку суррогатных пар CESU-8 в UTF-8 и неизменность остального.
func TestFixCESU8(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []byte
		want  []byte
	}{
		{name: "plain utf-8", input: []byte("Котёл ♨️ 20 °C"), want: []byte("Котёл ♨️ 20 °C")},
		{name: "surrogate pair", input: cesuFire, want: []byte("🔥")},
		{name: "pair inside text", input: append(append([]byte("Котёл "), cesuFire...), []byte(" горит")...), want: []byte("Котёл 🔥 горит")},
		{name: "two pairs", input: append(append([]byte{}, cesuFire...), cesuFire...), want: []byte("🔥🔥")},
		{name: "valid 4-byte utf-8 untouched", input: []byte("🔥"), want: []byte("🔥")},
		{name: "lone high surrogate untouched", input: []byte{'a', 0xED, 0xA0, 0xBD, 'b'}, want: []byte{'a', 0xED, 0xA0, 0xBD, 'b'}},
		{name: "lone low surrogate untouched", input: []byte{0xED, 0xB4, 0xA5}, want: []byte{0xED, 0xB4, 0xA5}},
		{name: "hangul U+D7FB untouched", input: []byte("ퟻ"), want: []byte("ퟻ")},
		{name: "truncated tail untouched", input: []byte{0xED, 0xA0, 0xBD, 0xED, 0xB4}, want: []byte{0xED, 0xA0, 0xBD, 0xED, 0xB4}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := fixCESU8(tt.input)
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("fixCESU8(% x) = % x (%q), want %q", tt.input, got, got, tt.want)
			}
		})
	}
}

// TestHandleSendFixesEmojiFromRules проверяет, что эмодзи из wb-rules доходят до Telegram целыми.
func TestHandleSendFixesEmojiFromRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "plain text", payload: append(append([]byte{}, cesuFire...), []byte(" Котёл")...)},
		{name: "json", payload: append(append([]byte(`{"text":"`), cesuFire...), []byte(` Котёл","id":"wbr-1"}`)...)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, deps := newTestBot(t)

			deps.sender.EXPECT().Send(int64(100), "🔥 Котёл").Return(nil)
			deps.publisher.EXPECT().SetValue("send", "🔥 Котёл").Return(nil)

			b.HandleSend(tt.payload)
		})
	}
}
