// Package qrcode generates QR code PNGs for listing detail URLs.
package qrcode

import (
	"fmt"
	"os"

	qr "github.com/skip2/go-qrcode"
)

// PNG renders content as a QR code PNG, size pixels per side, medium error
// correction, with the quiet zone the scanner needs.
func PNG(content string, size int) ([]byte, error) {
	if content == "" {
		return nil, fmt.Errorf("qrcode: empty content")
	}
	code, err := qr.New(content, qr.Medium)
	if err != nil {
		return nil, fmt.Errorf("qrcode: build: %w", err)
	}
	png, err := code.PNG(size)
	if err != nil {
		return nil, fmt.Errorf("qrcode: encode png: %w", err)
	}
	return png, nil
}

// WritePNG renders content as a QR code PNG of the given pixel size to path.
func WritePNG(content, path string, size int) error {
	png, err := PNG(content, size)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, png, 0o644); err != nil {
		return fmt.Errorf("qrcode: write %s: %w", path, err)
	}
	return nil
}
