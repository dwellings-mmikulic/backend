package api

import (
	"testing"
	"time"
)

func TestCursorRoundTrip_Newest(t *testing.T) {
	ts := time.Date(2026, 7, 1, 12, 30, 0, 0, time.UTC)
	tok := encodeCursor(cursor{Sort: "newest", ID: 42, CreatedAt: &ts})

	got, err := decodeCursor(tok, "newest")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 42 || got.CreatedAt == nil || !got.CreatedAt.Equal(ts) {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func TestCursorRoundTrip_Price(t *testing.T) {
	tok := encodeCursor(cursor{Sort: "price_asc", ID: 7, Price: 500000})

	got, err := decodeCursor(tok, "price_asc")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 7 || got.Price != 500000 {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func TestDecodeCursor_RejectsGarbage(t *testing.T) {
	for _, tok := range []string{"not base64 !!!", "aGVsbG8", ""} {
		if _, err := decodeCursor(tok, "newest"); err == nil {
			t.Errorf("token %q: want error, got nil", tok)
		}
	}
}

func TestDecodeCursor_RejectsSortMismatch(t *testing.T) {
	tok := encodeCursor(cursor{Sort: "price_asc", ID: 7, Price: 1})
	if _, err := decodeCursor(tok, "newest"); err == nil {
		t.Error("cursor from a different sort must be rejected")
	}
}

func TestDecodeCursor_RejectsNewestWithoutTimestamp(t *testing.T) {
	tok := encodeCursor(cursor{Sort: "newest", ID: 7}) // no CreatedAt
	if _, err := decodeCursor(tok, "newest"); err == nil {
		t.Error("newest cursor without timestamp must be rejected")
	}
}

func TestDecodeCursor_RejectsZeroID(t *testing.T) {
	ts := time.Now()
	tok := encodeCursor(cursor{Sort: "newest", CreatedAt: &ts}) // ID 0
	if _, err := decodeCursor(tok, "newest"); err == nil {
		t.Error("cursor without id must be rejected")
	}
}
