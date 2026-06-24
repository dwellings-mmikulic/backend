// Package qrcode generates QR code PNGs for listing detail URLs.
package qrcode

import (
	"fmt"
	"os"

	qr "github.com/skip2/go-qrcode"
)

// WritePNG renders content as a QR code PNG of the given pixel size to path.
func WritePNG(content, path string, size int) error {
	if content == "" {
		return fmt.Errorf("qrcode: empty content")
	}
	code, err := qr.New(content, qr.Medium)
	if err != nil {
		return fmt.Errorf("qrcode: build: %w", err)
	}
	// White quiet zone on transparent-friendly white background; readable scanned
	// off a TV screen.
	png, err := code.PNG(size)
	if err != nil {
		return fmt.Errorf("qrcode: encode png: %w", err)
	}
	if err := os.WriteFile(path, png, 0o644); err != nil {
		return fmt.Errorf("qrcode: write %s: %w", path, err)
	}
	return nil
}
