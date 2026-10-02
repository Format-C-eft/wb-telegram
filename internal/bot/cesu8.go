package bot

import (
	"bytes"
	"unicode/utf8"
)

// cesuLead — первый байт трёхбайтовой записи суррогата UTF-16 (U+D800..U+DFFF).
const cesuLead = 0xED

// fixCESU8 пересобирает в UTF-8 символы вне базовой плоскости Unicode (эмодзи и т.п.),
// которые wb-rules отдаёт в CESU-8: суррогатной парой из двух трёхбайтовых
// последовательностей вместо одной четырёхбайтовой. Одиночные суррогаты и всё
// остальное остаются как есть.
func fixCESU8(data []byte) []byte {
	if !bytes.Contains(data, []byte{cesuLead}) {
		return data
	}

	out := make([]byte, 0, len(data))

	for i := 0; i < len(data); {
		high, okHigh := decodeSurrogate(data[i:], 0xA0)
		low, okLow := decodeSurrogate(sliceFrom(data, i+3), 0xB0)

		if okHigh && okLow {
			out = utf8.AppendRune(out, 0x10000+(high-0xD800)<<10+(low-0xDC00))
			i += 6

			continue
		}

		out = append(out, data[i])
		i++
	}

	return out
}

// decodeSurrogate разбирает трёхбайтовый суррогат в начале data; second — 0xA0 для старшего
// (U+D800..U+DBFF) или 0xB0 для младшего (U+DC00..U+DFFF).
func decodeSurrogate(data []byte, second byte) (rune, bool) {
	if len(data) < 3 || data[0] != cesuLead || data[1]&0xF0 != second || data[2]&0xC0 != 0x80 {
		return 0, false
	}

	return 0xD000 | rune(data[1]&0x3F)<<6 | rune(data[2]&0x3F), true
}

// sliceFrom возвращает data[from:] или пустой срез, если from за концом.
func sliceFrom(data []byte, from int) []byte {
	if from >= len(data) {
		return nil
	}

	return data[from:]
}
