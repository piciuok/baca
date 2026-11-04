package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

const iconPx = 32

// Icon draws the day: a thin full ring and an arc for the share of the target worked.
// template (macOS menu bar) is black on transparent; otherwise blue, green when the target
// is reached and fully logged.
func Icon(frac float64, done, template bool) []byte {
	frac = math.Max(0, math.Min(1, frac))
	track := color.NRGBA{0x9A, 0xA0, 0xA6, 0x90}
	arc := color.NRGBA{0x2F, 0x81, 0xF7, 0xFF}
	if done {
		arc = color.NRGBA{0x3D, 0xBE, 0x8B, 0xFF}
	}
	if template {
		track, arc = color.NRGBA{0, 0, 0, 0x55}, color.NRGBA{0, 0, 0, 0xFF}
	}
	const ss = 4
	img := image.NewNRGBA(image.Rect(0, 0, iconPx, iconPx))
	for py := 0; py < iconPx; py++ {
		for px := 0; px < iconPx; px++ {
			var cover [2]float64 // track, arc
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / iconPx
					y := (float64(py) + (float64(sy)+0.5)/ss) / iconPx
					dx, dy := x-0.5, y-0.5
					d := math.Hypot(dx, dy)
					if d < 0.30 || d > 0.46 {
						if d < 0.12 && done { // a dot in the middle: day closed
							cover[1]++
						}
						continue
					}
					// angle clockwise from 12 o'clock, 0..1
					a := math.Atan2(dx, -dy) / (2 * math.Pi)
					if a < 0 {
						a++
					}
					if a <= frac && frac > 0 {
						cover[1]++
					} else if d > 0.34 && d < 0.42 {
						cover[0]++
					}
				}
			}
			n := float64(ss * ss)
			var c color.NRGBA
			switch {
			case cover[1] > 0:
				c = arc
				c.A = uint8(float64(arc.A) * cover[1] / n)
			case cover[0] > 0:
				c = track
				c.A = uint8(float64(track.A) * cover[0] / n)
			}
			img.SetNRGBA(px, py, c)
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}
