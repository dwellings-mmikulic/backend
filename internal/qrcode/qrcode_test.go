package qrcode

import (
	"bytes"
	"image/png"
	"testing"
)

func TestPNG_RendersSquareImage(t *testing.T) {
	b, err := PNG("https://dwellings.tv/tv/0123456789abcdef0123456789abcdef", 300)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	if r := img.Bounds(); r.Dx() != 300 || r.Dy() != 300 {
		t.Errorf("size = %dx%d, want 300x300", r.Dx(), r.Dy())
	}
}

func TestPNG_RejectsEmptyContent(t *testing.T) {
	if _, err := PNG("", 300); err == nil {
		t.Error("empty content accepted")
	}
}
