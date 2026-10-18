package feed

import (
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/property"
)

func TestCursor_RoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 34, 56, 789000000, time.UTC)
	p := property.Property{ID: 4242, VideoRenderedAt: &at}
	tok := EncodeCursor(&p)
	if tok == "" {
		t.Fatal("empty token")
	}
	key, err := DecodeCursor(tok)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if !key.RenderedAt.Equal(at) || key.ID != 4242 {
		t.Errorf("key = %+v, want rendered_at %v id 4242", key, at)
	}
}

func TestDecodeCursor_Rejects(t *testing.T) {
	for _, tok := range []string{"", "not base64!", "e30", "eyJpZCI6MH0"} {
		if _, err := DecodeCursor(tok); err != ErrInvalidCursor {
			t.Errorf("DecodeCursor(%q) err = %v, want ErrInvalidCursor", tok, err)
		}
	}
}
