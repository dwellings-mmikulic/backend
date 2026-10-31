package linear

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

// AnchoredPlaylist renders household h's feed as the continuation of
// channel sc, for a platform that fetches the channel with the viewer's ip=
// (see the design spec, "Anchoring"). The first call for (h, sc) creates
// the anchor: the switch point S is the end of the channel segment on air,
// and the offsets make the first personal segment after S carry the
// counters the channel's next segment would have. Every call then serves
// the channel's segments that ended by S (while S is still within the
// window) followed by the household's from S on, relabelled.
//
// ErrUnknownHousehold when h has no feed: the caller serves the channel.
func (s *Service) AnchoredPlaylist(ctx context.Context, h viewer.ID, sc Scope, now time.Time) ([]byte, error) {
	if err := s.checkArea(ctx, sc); err != nil {
		return nil, err
	}
	spans, err := s.Household(ctx, h)
	if err != nil {
		return nil, err
	}
	key := sc.Key()
	a, err := s.cachedAnchor(ctx, h, key)
	if err != nil {
		return nil, err
	}
	if a == nil {
		if a, err = s.anchor(ctx, h, spans, key, now); err != nil {
			return nil, err
		}
	}
	var segs []segment
	if !a.StartsAt.Before(now.Add(-lookback)) {
		// The channel up to S — or up to now while S is still ahead, so
		// nothing unfinished is listed.
		upTo := a.StartsAt
		if now.Before(upTo) {
			upTo = now
		}
		segs, err = s.channelWindow(ctx, key, upTo)
		if err != nil {
			return nil, err
		}
	}
	personal, _, err := s.personalSegments(ctx, spans, now)
	if err != nil {
		return nil, err
	}
	i := 0
	for i < len(personal) && personal[i].Start.Before(a.StartsAt) {
		i++
	}
	segs = append(segs, shifted(personal[i:], Span{N: 1, SeqOffset: a.SeqOffset, ItemOffset: a.ItemOffset})...)
	if len(segs) == 0 {
		return nil, ErrNoContent
	}
	var buf bytes.Buffer
	writePlaylist(&buf, liveWindow.trim(segs))
	return buf.Bytes(), nil
}

// cachedAnchor is h's anchor on channel key, or nil. An anchor is written
// once and never changes, so one found is served from memory for
// householdTTL; nil is not cached, the caller creates the anchor at once.
func (s *Service) cachedAnchor(ctx context.Context, h viewer.ID, key string) (*Anchor, error) {
	k := anchorKey{h, key}
	if a, ok := s.anchors.get(k); ok {
		return a, nil
	}
	a, err := s.store.Anchor(ctx, h, key)
	if err != nil {
		return nil, err
	}
	if a != nil {
		s.anchors.put(k, a)
	}
	return a, nil
}

// anchor creates and stores h's anchor on channel key at now, then re-reads
// it so a concurrent creator's row wins.
func (s *Service) anchor(ctx context.Context, h viewer.ID, spans []Span, key string, now time.Time) (*Anchor, error) {
	S, err := s.endOfAiring(ctx, key, now)
	if err != nil {
		return nil, err
	}
	before, err := s.channelWindow(ctx, key, S)
	if err != nil {
		return nil, err
	}
	if len(before) == 0 {
		return nil, fmt.Errorf("channel %s: nothing aired before %s", key, S)
	}
	last := before[len(before)-1]
	first, err := s.firstPersonalAtOrAfter(ctx, spans, S)
	if err != nil {
		return nil, err
	}
	a := &Anchor{Household: h, Channel: key, StartsAt: S,
		SeqOffset: last.Seq + 1 - first.Seq, ItemOffset: last.Item + 1 - first.Item, CreatedAt: now}
	if _, err := s.store.InsertAnchor(ctx, a); err != nil {
		return nil, err
	}
	stored, err := s.store.Anchor(ctx, h, key)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("household %s: anchor on %s vanished after insert", h, key)
	}
	s.anchors.put(anchorKey{h, key}, stored)
	s.log.Info("channel anchor created", "household", h.String(), "channel", key, "starts_at", S)
	return stored, nil
}
