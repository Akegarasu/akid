package tui

import (
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

func textCellColumn(text string, runeColumn int) int {
	graphemes := uniseg.NewGraphemes(text)
	runes, cells := 0, 0
	for graphemes.Next() {
		next := runes + utf8.RuneCountInString(graphemes.Str())
		if runeColumn < next {
			return cells
		}
		cells += graphemes.Width()
		runes = next
	}
	return cells
}

func textRuneAtCell(text string, cell int) int {
	graphemes := uniseg.NewGraphemes(text)
	runes, cells := 0, 0
	for graphemes.Next() {
		cells += graphemes.Width()
		if cell < cells {
			return runes
		}
		runes += utf8.RuneCountInString(graphemes.Str())
	}
	return runes
}

func moveTextColumn(text string, column, direction int) int {
	graphemes := uniseg.NewGraphemes(text)
	start := 0
	for graphemes.Next() {
		end := start + utf8.RuneCountInString(graphemes.Str())
		if direction < 0 && column <= end {
			return start
		}
		if direction > 0 && column < end {
			return end
		}
		start = end
	}
	return start
}
