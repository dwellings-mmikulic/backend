package linear

// A bumper is a short clip of our own (the QR invitation, see
// docs/superpowers/specs/2026-10-13-qr-bumper-design.md) that the lineup
// builder inserts between listings. Lineups reference clips by video_hls id
// and bumpers by the NEGATED bumpers id in the same item list, so the chain
// schema and every counter stay as they are. These three helpers are the
// only code that knows the sign convention.

// bumperItem is the lineup item id of bumper id.
func bumperItem(id int64) int64 { return -id }

// isBumper reports whether a lineup item references a bumper.
func isBumper(item int64) bool { return item < 0 }

// bumperID is the bumpers id behind a bumper lineup item.
func bumperID(item int64) int64 { return -item }

// ref is the bumper as the lineup builder sees it.
func (b *Bumper) ref() ClipRef {
	return ClipRef{ID: bumperItem(b.ID), TotalMS: b.TotalMS, Segments: len(b.SegmentMS)}
}
