package feed

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/dwellingtw/backend/internal/property"
)

// ErrInvalidCursor is returned for any undecodable or incomplete feed page
// token. Its text is the public 400 message.
var ErrInvalidCursor = errors.New("invalid cursor")

// cursor is the decoded page token: the render time and id of the last
// listing on the previous page, the feed's sort key.
type cursor struct {
	RenderedAt time.Time `json:"t"`
	ID         int64     `json:"id"`
}

// EncodeCursor renders the page token that continues after p: an opaque,
// URL-safe string the Roku app passes back as ?cursor=.
func EncodeCursor(p *property.Property) string {
	c := cursor{ID: p.ID}
	if p.VideoRenderedAt != nil {
		c.RenderedAt = *p.VideoRenderedAt
	}
	b, _ := json.Marshal(c) // struct of scalars — cannot fail
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses a page token into the keyset position it names.
func DecodeCursor(token string) (*property.FeedKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil || c.ID == 0 || c.RenderedAt.IsZero() {
		return nil, ErrInvalidCursor
	}
	return &property.FeedKey{RenderedAt: c.RenderedAt, ID: c.ID}, nil
}
