package tray

import (
	"bytes"
	"image/png"
	"os"
	"testing"
)

func TestIcon(t *testing.T) {
	for _, c := range []struct {
		frac           float64
		done, template bool
	}{{0, false, true}, {0.4, false, false}, {1.2, true, false}} {
		img, err := png.Decode(bytes.NewReader(Icon(c.frac, c.done, c.template)))
		if err != nil || img.Bounds().Dx() != iconPx {
			t.Fatal(err)
		}
		// top center is on the ring: drawn as soon as any share is worked, track otherwise
		_, _, _, a := img.At(iconPx/2, 2).RGBA()
		if a == 0 {
			t.Fatalf("%+v: nothing at the top of the ring", c)
		}
	}
	if dir := os.Getenv("BACA_ICONS"); dir != "" { // preview: BACA_ICONS=/tmp/x go test
		for i, f := range []float64{0, 0.25, 0.6, 1} {
			os.WriteFile(dir+"/icon"+string(rune('0'+i))+".png", Icon(f, f >= 1, false), 0o644)
			os.WriteFile(dir+"/tmpl"+string(rune('0'+i))+".png", Icon(f, f >= 1, true), 0o644)
		}
	}
}
