package imaging

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

// opaqueImage returns a small fully-opaque test image.
func opaqueImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := range 24 {
		for x := range 32 {
			img.Set(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 10), B: 200, A: 255})
		}
	}
	return img
}

// transparentImage returns a small image with partial transparency.
func transparentImage() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 24))
	for y := range 24 {
		for x := range 32 {
			img.Set(x, y, color.NRGBA{R: 255, A: 128})
		}
	}
	return img
}

func encode(t *testing.T, enc func(*bytes.Buffer) error) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := enc(&buf); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestNormalize_JPEGPassthrough(t *testing.T) {
	in := encode(t, func(b *bytes.Buffer) error { return jpeg.Encode(b, opaqueImage(), nil) })
	out, ext, ct, err := Normalize(in)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Error("JPEG input must pass through byte-identical (no recompression)")
	}
	if ext != ".jpg" || ct != "image/jpeg" {
		t.Errorf("got ext=%q ct=%q, want .jpg image/jpeg", ext, ct)
	}
}

func TestNormalize_PNGPassthrough(t *testing.T) {
	in := encode(t, func(b *bytes.Buffer) error { return png.Encode(b, transparentImage()) })
	out, ext, ct, err := Normalize(in)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Error("PNG input must pass through byte-identical")
	}
	if ext != ".png" || ct != "image/png" {
		t.Errorf("got ext=%q ct=%q, want .png image/png", ext, ct)
	}
}

func TestNormalize_OpaqueWebPBecomesJPEG(t *testing.T) {
	out, ext, ct, err := Normalize(readFixture(t, "opaque.webp"))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ext != ".jpg" || ct != "image/jpeg" {
		t.Fatalf("got ext=%q ct=%q, want .jpg image/jpeg", ext, ct)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not valid JPEG: %v", err)
	}
	if img.Bounds().Dx() != 32 || img.Bounds().Dy() != 24 {
		t.Errorf("dimensions changed: got %v, want 32x24", img.Bounds())
	}
}

func TestNormalize_TransparentWebPBecomesPNG(t *testing.T) {
	out, ext, ct, err := Normalize(readFixture(t, "transparent.webp"))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ext != ".png" || ct != "image/png" {
		t.Fatalf("got ext=%q ct=%q, want .png image/png", ext, ct)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not valid PNG: %v", err)
	}
	if opaque(img) {
		t.Error("transparency was lost in transcode")
	}
}

func TestNormalize_GIFBecomesJPEG(t *testing.T) {
	in := encode(t, func(b *bytes.Buffer) error { return gif.Encode(b, opaqueImage(), nil) })
	out, ext, ct, err := Normalize(in)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ext != ".jpg" || ct != "image/jpeg" {
		t.Fatalf("got ext=%q ct=%q, want .jpg image/jpeg", ext, ct)
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("output is not valid JPEG: %v", err)
	}
}

func TestNormalize_TransparentGIFBecomesPNG(t *testing.T) {
	pal := image.NewPaletted(image.Rect(0, 0, 8, 8), color.Palette{
		color.RGBA{}, // fully transparent
		color.RGBA{R: 255, A: 255},
	})
	in := encode(t, func(b *bytes.Buffer) error { return gif.Encode(b, pal, nil) })
	_, ext, _, err := Normalize(in)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ext != ".png" {
		t.Errorf("transparent GIF should become PNG, got %q", ext)
	}
}

func TestNormalize_BMPBecomesJPEG(t *testing.T) {
	in := encode(t, func(b *bytes.Buffer) error { return bmp.Encode(b, opaqueImage()) })
	_, ext, _, err := Normalize(in)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ext != ".jpg" {
		t.Errorf("got ext=%q, want .jpg", ext)
	}
}

func TestNormalize_TIFFBecomesJPEG(t *testing.T) {
	in := encode(t, func(b *bytes.Buffer) error { return tiff.Encode(b, opaqueImage(), nil) })
	_, ext, _, err := Normalize(in)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ext != ".jpg" {
		t.Errorf("got ext=%q, want .jpg", ext)
	}
}

func TestNormalize_GarbageErrors(t *testing.T) {
	for name, data := range map[string][]byte{
		"garbage": []byte("this is not an image at all, just text bytes"),
		"empty":   {},
		"html":    []byte("<html><body>404 not found</body></html>"),
	} {
		if _, _, _, err := Normalize(data); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
