package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

const iconPx = 32

// Icon draws the day as a ring around baca's hat: a thin track and an arc for the share of the
// target worked. template (macOS menu bar) is black on transparent with the hat band cut out;
// otherwise the arc is blue, green once the target is reached and fully logged.
func Icon(frac float64, done, template bool) []byte {
	frac = math.Max(0, math.Min(1, frac))
	track := color.NRGBA{0x9A, 0xA0, 0xA6, 0x90}
	arc := color.NRGBA{0x2F, 0x81, 0xF7, 0xFF}
	if done {
		arc = color.NRGBA{0x3D, 0xBE, 0x8B, 0xFF}
	}
	hat := color.NRGBA{0x8A, 0x91, 0x9C, 0xFF}
	band := color.NRGBA{0x2B, 0x2F, 0x36, 0xFF}
	if template {
		track, arc, hat = color.NRGBA{0, 0, 0, 0x55}, color.NRGBA{0, 0, 0, 0xFF}, color.NRGBA{0, 0, 0, 0xFF}
	}
	const ss = 4
	img := image.NewNRGBA(image.Rect(0, 0, iconPx, iconPx))
	for py := 0; py < iconPx; py++ {
		for px := 0; px < iconPx; px++ {
			var cover [4]float64 // track, arc, hat, band
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / iconPx
					y := (float64(py) + (float64(sy)+0.5)/ss) / iconPx
					dx, dy := x-0.5, y-0.5
					d := math.Hypot(dx, dy)
					switch {
					case d >= 0.38 && d <= 0.48:
						// angle clockwise from 12 o'clock, 0..1
						a := math.Atan2(dx, -dy) / (2 * math.Pi)
						if a < 0 {
							a++
						}
						if frac > 0 && a <= frac {
							cover[1]++
						} else if d > 0.40 && d < 0.46 {
							cover[0]++
						}
					case inHat(x, y):
						if inBand(x, y) {
							cover[3]++
						} else {
							cover[2]++
						}
					}
				}
			}
			n := float64(ss * ss)
			var c color.NRGBA
			pick := func(col color.NRGBA, cov float64) {
				c = col
				c.A = uint8(float64(col.A) * cov / n)
			}
			switch {
			case cover[1] > 0:
				pick(arc, cover[1])
			case cover[0] > 0:
				pick(track, cover[0])
			case cover[2] > 0 && template:
				pick(hat, cover[2]) // the band stays transparent: a cut-out
			case cover[2]+cover[3] > 0:
				if cover[3] > cover[2] {
					pick(band, cover[2]+cover[3])
				} else {
					pick(hat, cover[2]+cover[3])
				}
			}
			img.SetNRGBA(px, py, c)
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

// inHat: a low round crown on a wide brim, in the unit square.
func inHat(x, y float64) bool {
	brim := math.Pow((x-0.5)/0.30, 2)+math.Pow((y-0.635)/0.075, 2) <= 1
	crown := y <= 0.64 && math.Pow((x-0.5)/0.165, 2)+math.Pow((y-0.64)/0.25, 2) <= 1
	return brim || crown
}

// inBand: the band of shells around the bottom of the crown.
func inBand(x, y float64) bool {
	return y >= 0.53 && y <= 0.59 && math.Pow((x-0.5)/0.165, 2)+math.Pow((y-0.64)/0.25, 2) <= 1
}
