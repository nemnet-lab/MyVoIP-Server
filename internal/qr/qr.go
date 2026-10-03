// Package qr は登録用 URL をターミナルに QR として表示する。
package qr

import (
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// Terminal は上下2モジュールを1文字にまとめた QR を返す（白地に黒）。
func Terminal(text string) (string, error) {
	code, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return "", err
	}
	bitmap := code.Bitmap() // true = 黒
	var sb strings.Builder
	for y := 0; y < len(bitmap); y += 2 {
		for x := 0; x < len(bitmap[y]); x++ {
			top := bitmap[y][x]
			bottom := y+1 < len(bitmap) && bitmap[y+1][x]
			switch {
			case top && bottom:
				sb.WriteString(" ")
			case top:
				sb.WriteString("▄")
			case bottom:
				sb.WriteString("▀")
			default:
				sb.WriteString("█")
			}
		}
		sb.WriteString("\n")
	}
	return sb.String(), nil
}
