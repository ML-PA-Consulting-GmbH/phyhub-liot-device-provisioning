package main

import (
	"strings"
)

// boxBorder returns the top/bottom border of a box of the given inner width.
// `width` is the number of inner characters (between the two `+`).
func boxBorder(width int) string {
	return "+" + strings.Repeat("-", width) + "+"
}

// boxLine returns a single banner line: `|`, content centered in `width`
// characters, `|`. If content is too long it is truncated. Self-aligns so
// callers can't produce off-by-one width errors visually.
func boxLine(content string, width int) string {
	if len(content) > width {
		content = content[:width]
	}
	pad := width - len(content)
	left := pad / 2
	right := pad - left
	return "|" + strings.Repeat(" ", left) + content + strings.Repeat(" ", right) + "|"
}
