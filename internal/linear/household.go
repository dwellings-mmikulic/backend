package linear

import (
	"context"
	"errors"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

// ErrUnknownHousehold reports a household id no feed was ever created for.
var ErrUnknownHousehold = errors.New("unknown household")

// citiesTTL is how long Cities reuses its answer: the list changes only as
// the library grows, and the mobile page fetches it on every open.
const citiesTTL = 10 * time.Minute

// Household returns h's spans, oldest first.
func (s *Service) Household(ctx context.Context, h viewer.ID) ([]Span, error) {
	spans, err := s.store.Spans(ctx, h)
	if err != nil {
		return nil, err
	}
	if len(spans) == 0 {
		return nil, ErrUnknownHousehold
	}
	return spans, nil
}

// EnsureHousehold returns h's spans, creating span 0 at now when h is new.
// The default channel is the first candidate (the IP's ZIP, city, state —
// see geoCandidates) that resolves to something more specific than
// national, else national. Span 0 has zero offsets: a fresh feed's counters
// are its channel's own.
func (s *Service) EnsureHousehold(ctx context.Context, h viewer.ID, candidates []Scope, now time.Time) ([]Span, error) {
	spans, err := s.store.Spans(ctx, h)
	if err != nil {
		return nil, err
	}
	if len(spans) > 0 {
		return spans, nil
	}
	sp := Span{Household: h, N: 0, Scope: Scope{}.Key(), Source: SourceDefault, StartsAt: now, CreatedAt: now}
	for _, c := range candidates {
		eff, _, err := s.resolveScope(ctx, c)
		if err != nil {
			if errors.Is(err, ErrNoContent) {
				continue
			}
			return nil, err
		}
		if eff != (Scope{}) {
			sp.Scope, sp.Requested, sp.Source = eff.Key(), c.Key(), SourceGeo
			break
		}
	}
	if _, err := s.store.InsertSpan(ctx, &sp); err != nil {
		return nil, err
	}
	// Re-read: a concurrent first request may have won the insert, and its
	// row is the one every later request will see.
	return s.Household(ctx, h)
}

// Cities lists the areas with enough content for a channel of their own.
func (s *Service) Cities(ctx context.Context) ([]City, error) {
	s.citiesMu.Lock()
	defer s.citiesMu.Unlock()
	if s.cities != nil && s.now().Sub(s.citiesAt) < citiesTTL {
		return s.cities, nil
	}
	cs, err := s.store.ListCities(ctx, s.opts.MinScopeClips)
	if err != nil {
		return nil, err
	}
	if cs == nil {
		cs = []City{}
	}
	s.cities, s.citiesAt = cs, s.now()
	return cs, nil
}
