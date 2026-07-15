// Package api serves the public read-only listings API.
package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// errInvalidCursor is returned for any undecodable, mismatched, or
// incomplete pagination token. Its text is the public 400 message.
var errInvalidCursor = errors.New("invalid cursor")

// cursor is the decoded keyset pagination token: the sort it belongs to and
// the sort-key values of the last row of the previous page.
type cursor struct {
	Sort      string     `json:"s"`
	ID        int64      `json:"id"`
	Price     int64      `json:"p,omitempty"`
	CreatedAt *time.Time `json:"t,omitempty"`
}

// encodeCursor renders the cursor as an opaque URL-safe token.
func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c) // struct of scalars — cannot fail
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses a token and validates it against the request's sort.
func decodeCursor(token, wantSort string) (*cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, errInvalidCursor
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errInvalidCursor
	}
	if c.Sort != wantSort || c.ID == 0 {
		return nil, errInvalidCursor
	}
	if c.Sort == "newest" && c.CreatedAt == nil {
		return nil, errInvalidCursor
	}
	return &c, nil
}
