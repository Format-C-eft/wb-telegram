package chart

import (
	"image"
	"image/color"
	"image/draw"
)

// digits содержит собственные геометрические глифы 5x7.
var digits = map[rune][7]byte{
	'0': {14, 17, 19, 21, 25, 17, 14},
	'1': {4, 12, 4, 4, 4, 4, 14},
	'2': {14, 17, 1, 2, 4, 8, 31},
	'3': {30, 1, 1, 14, 1, 1, 30},
	'4': {2, 6, 10, 18, 31, 2, 2},
	'5': {31, 16, 16, 30, 1, 1, 30},
	'6': {14, 16, 16, 30, 17, 17, 14},
	'7': {31, 1, 2, 4, 8, 8, 8},
	'8': {14, 17, 17, 14, 17, 17, 14},
	'9': {14, 17, 17, 15, 1, 1, 14},
	'-': {0, 0, 0, 31, 0, 0, 0},
	'+': {0, 4, 4, 31, 4, 4, 0},
	'.': {0, 0, 0, 0, 0, 6, 6},
	':': {0, 6, 6, 0, 6, 6, 0},
	'e': {0, 0, 14, 17, 31, 16, 14},
	' ': {0, 0, 0, 0, 0, 0, 0},
}

// text рисует числовую подпись собственным bitmap-шрифтом.
func text(canvas *image.RGBA, x, y int, value string, shade color.RGBA, scale int) {
	for _, character := range value {
		glyph := digits[character]

		for row, bits := range glyph {
			for column := range 5 {
				if bits&(1<<uint(4-column)) != 0 {
					draw.Draw(canvas, image.Rect(x+column*scale, y+row*scale, x+(column+1)*scale, y+(row+1)*scale), image.NewUniform(shade), image.Point{}, draw.Src)
				}
			}
		}

		x += 6 * scale
	}
}
