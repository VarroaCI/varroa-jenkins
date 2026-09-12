package recordstore

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// eventKeySeparator joins EventKey's fields. It is the U+001F
// information-separator, matching frontend/src/components/activityTimeline.util.ts.
// The two keys are computed independently and never compared, so a future
// divergence in the browser cannot corrupt a row.
const eventKeySeparator = "\x1f"

// EventKey computes the activity table's primary-key half that the
// recorder derives, never a producer. It is stable across redeliveries,
// so ingest can insert once and ignore a duplicate. The timestamp is
// normalized because a producer's raw string is not stable across
// encoders.
func EventKey(e activity.Event) string {
	build := ""
	if e.BuildNumber != 0 {
		build = strconv.FormatInt(e.BuildNumber, 10)
	}
	parts := []string{
		e.Cluster,
		e.Timestamp.UTC().Format(time.RFC3339Nano),
		e.Source, e.Type, e.Namespace, e.Controller, build, e.Message,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, eventKeySeparator)))
	return hex.EncodeToString(sum[:])
}
