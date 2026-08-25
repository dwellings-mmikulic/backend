package linear

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPlaylist_IsDeterministicAcrossInstances(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	now := t0.Add(90 * time.Second)
	a := testService(m, now)
	b := testService(m, now)

	pa, err := a.Playlist(context.Background(), Scope{Zip: "77494"}, now)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := b.Playlist(context.Background(), Scope{Zip: "77494"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pa, pb) {
		t.Errorf("instances disagree:\n%s\n---\n%s", pa, pb)
	}
	if !strings.HasPrefix(string(pa), "#EXTM3U\n") || strings.Count(string(pa), "#EXTINF:") != windowSegments {
		t.Errorf("unexpected playlist:\n%s", pa)
	}
}

func TestPlaylist_AdvancesWithTime(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	p1, err := s.Playlist(context.Background(), Scope{Zip: "77494"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.Playlist(context.Background(), Scope{Zip: "77494"}, t0.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(p1, p2) {
		t.Error("playlist did not advance after 30 s")
	}
}

func TestPlaylist_NoContent(t *testing.T) {
	s := testService(newMemStore(), t0)
	if _, err := s.Playlist(context.Background(), Scope{}, t0); !errors.Is(err, ErrNoContent) {
		t.Errorf("err = %v, want ErrNoContent", err)
	}
}
