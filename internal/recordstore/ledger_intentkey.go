package recordstore

import (
	"errors"
	"fmt"
	"strings"
)

// IntentScheme names one of the seven allocated intent-key schemes.
type IntentScheme string

// The seven allocated intent-key schemes. The withdrawn "webhook" scheme
// does not parse.
const (
	IntentSchemeSCM      IntentScheme = "scm"
	IntentSchemeManual   IntentScheme = "manual"
	IntentSchemeCron     IntentScheme = "cron"
	IntentSchemeRetry    IntentScheme = "retry"
	IntentSchemeUpstream IntentScheme = "upstream"
	IntentSchemeAPI      IntentScheme = "api"
	// IntentSchemeReplay keys a replayed build as
	// replay:<source run id>:<new run id>. The second segment is the new
	// run's own UUIDv7, never a counter, so two replays never share a key.
	IntentSchemeReplay IntentScheme = "replay"
)

// maxIntentKeyBytes is the length cap D1 places on run_intents.intent_key.
const maxIntentKeyBytes = 512

// ErrInvalidIntentKey is the error ParseIntentKey returns for an unknown
// scheme, an empty segment, or a key over the length cap. The recorder
// maps it to reason_code=INVALID_KEY.
var ErrInvalidIntentKey = errors.New("invalid intent key")

// intentKeyTailSegments maps a scheme to the number of non-empty
// segments its shape requires after the scheme token. A scheme's last
// segment absorbs every remaining colon, so a cron fire-minute keeps its
// own colons intact.
var intentKeyTailSegments = map[IntentScheme]int{
	IntentSchemeSCM:      2,
	IntentSchemeManual:   1,
	IntentSchemeCron:     2,
	IntentSchemeRetry:    1,
	IntentSchemeUpstream: 2,
	IntentSchemeAPI:      1,
	IntentSchemeReplay:   2,
}

// ParseIntentKey validates key against the seven allocated intent-key
// schemes and returns the matched scheme. It rejects an unknown scheme,
// an empty segment, and a key longer than 512 bytes.
func ParseIntentKey(key string) (IntentScheme, error) {
	if len(key) > maxIntentKeyBytes {
		return "", fmt.Errorf("%w: key exceeds %d bytes", ErrInvalidIntentKey, maxIntentKeyBytes)
	}

	scheme, rest, ok := strings.Cut(key, ":")
	if !ok {
		return "", fmt.Errorf("%w: key has no scheme", ErrInvalidIntentKey)
	}

	tail, known := intentKeyTailSegments[IntentScheme(scheme)]
	if !known {
		return "", fmt.Errorf("%w: unknown scheme %q", ErrInvalidIntentKey, scheme)
	}

	parts := strings.SplitN(rest, ":", tail)
	if len(parts) != tail {
		return "", fmt.Errorf("%w: wrong segment count for scheme %q", ErrInvalidIntentKey, scheme)
	}
	for _, part := range parts {
		if part == "" {
			return "", fmt.Errorf("%w: empty segment in scheme %q", ErrInvalidIntentKey, scheme)
		}
	}

	return IntentScheme(scheme), nil
}
