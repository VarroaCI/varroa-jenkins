package activity

import (
	"time"
)

// ParseRetention parses the retention dial string and returns the
// corresponding MaxAge duration, whether persistence is off, and whether
// the value was unrecognized (fallback to 7d).
//
// Mapping (single source of truth — no duration literals elsewhere):
//
//	"off" → (0, true, false)
//	"7d"  → (168h, false, false)
//	"30d" → (720h, false, false)
//	"90d" → (2160h, false, false)
//	"" / unknown → (168h, false, true)  // fallback with warning
func ParseRetention(s string) (maxAge time.Duration, off bool, fallback bool) {
	switch s {
	case "off":
		return 0, true, false
	case "7d":
		return 168 * time.Hour, false, false
	case "30d":
		return 720 * time.Hour, false, false
	case "90d":
		return 2160 * time.Hour, false, false
	default:
		// Empty or unrecognized → default to 7d with fallback flag.
		return 168 * time.Hour, false, true
	}
}
