// Package bunny uploads files to Bunny CDN Storage and returns public CDN URLs.
package bunny

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client uploads objects to a Bunny CDN storage zone.
type Client struct {
	storageZone string
	apiKey      string
	storageHost string
	cdnBaseURL  string
	http        *http.Client
}

// New creates a Bunny CDN storage client.
//   - storageHost: e.g. "storage.bunnycdn.com" or a regional host like "la.storage.bunnycdn.com"
//   - cdnBaseURL:  the public pull-zone base, e.g. "https://dwellings.b-cdn.net"
func New(storageZone, apiKey, storageHost, cdnBaseURL string, timeout time.Duration) *Client {
	return &Client{
		storageZone: storageZone,
		apiKey:      apiKey,
		storageHost: strings.TrimRight(storageHost, "/"),
		cdnBaseURL:  strings.TrimRight(cdnBaseURL, "/"),
		http:        &http.Client{Timeout: timeout},
	}
}

// Upload streams the given content to path within the storage zone and returns
// the public CDN URL. path should not start with a slash, e.g.
// "properties/12345/0.jpg".
func (c *Client) Upload(ctx context.Context, path string, content io.Reader, contentType string) (string, error) {
	path = strings.TrimLeft(path, "/")
	endpoint := fmt.Sprintf("https://%s/%s/%s", c.storageHost, c.storageZone, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, content)
	if err != nil {
		return "", fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("AccessKey", c.apiKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("bunny upload: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", fmt.Errorf("bunny upload returned status %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	return fmt.Sprintf("%s/%s", c.cdnBaseURL, path), nil
}
