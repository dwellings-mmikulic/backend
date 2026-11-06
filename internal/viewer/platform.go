package viewer

// platforms are the distribution partners that carry the channel under feed
// URLs of their own (/channels/<platform>/…), so their audience can be
// counted apart and their ad setup kept separate. Every name is a stored
// value and a route, so the list is closed: a new partner is a new entry.
var platforms = map[string]bool{
	"cineplex": true, // Cineplex Media's apps (the "Monthly Sponsor" row)
}

// KnownPlatform reports whether name is a platform's path segment. Names are
// matched exactly (lowercase), so each platform has one URL.
func KnownPlatform(name string) bool { return platforms[name] }
