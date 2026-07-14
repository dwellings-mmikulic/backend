// Package imaging guarantees property photos are stored as JPEG or PNG.
// JPEG/PNG input passes through untouched (no recompression, no quality
// loss); other decodable formats are transcoded. Format detection uses the
// image bytes themselves, never file extensions or Content-Type headers.
package imaging

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	_ "image/gif" // register GIF decoder

	_ "golang.org/x/image/bmp"  // register BMP decoder
	_ "golang.org/x/image/tiff" // register TIFF decoder
	_ "golang.org/x/image/webp" // register WebP decoder
)

// jpegQuality is used when transcoding non-JPEG/PNG sources. High enough to
// be visually lossless for listing photos.
const jpegQuality = 95

// Normalize returns image bytes guaranteed to be JPEG or PNG, along with the
// matching file extension (".jpg" or ".png") and content type. JPEG and PNG
// input is returned unchanged; WebP, GIF, BMP, and TIFF are transcoded to
// JPEG (or PNG when the image carries transparency). Undecodable input
// returns an error.
func Normalize(data []byte) (out []byte, ext, contentType string, err error) {
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", "", fmt.Errorf("detect image format: %w", err)
	}

	switch format {
	case "jpeg":
		return data, ".jpg", "image/jpeg", nil
	case "png":
		return data, ".png", "image/png", nil
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", "", fmt.Errorf("decode %s image: %w", format, err)
	}

	var buf bytes.Buffer
	if opaque(img) {
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, "", "", fmt.Errorf("encode %s as jpeg: %w", format, err)
		}
		return buf.Bytes(), ".jpg", "image/jpeg", nil
	}
	if err := png.Encode(&buf, img); err != nil {
		return nil, "", "", fmt.Errorf("encode %s as png: %w", format, err)
	}
	return buf.Bytes(), ".png", "image/png", nil
}

// opaque reports whether every pixel is fully opaque, preferring the image
// type's own Opaque() fast path when available.
func opaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return false
			}
		}
	}
	return true
}
