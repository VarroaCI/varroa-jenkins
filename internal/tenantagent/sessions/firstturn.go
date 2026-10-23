package sessions

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// maxEventMessage bounds the activity message that the first turn quotes.
const maxEventMessage = 1024

// TurnText builds the first user message of a session in the layout of design
// section 8.2. The instructions block is written when resolved is true, even
// for an empty file. path and revision name the instructions file.
func TurnText(sess agent.Session, e activity.Event, path, revision, body string, resolved bool) string {
	msg := e.Message
	if len(msg) > maxEventMessage {
		msg = msg[:maxEventMessage]
	}
	build := ""
	if n := buildOf(sess); n > 0 {
		build = strconv.FormatInt(n, 10)
	}
	run, _ := sess.Context["run"].(string)
	var b strings.Builder
	fmt.Fprintf(&b, "Trigger: %s on %s, tenant %s.\n", sess.Trigger, sess.Cluster, sess.Tenant)
	fmt.Fprintf(&b, "Item: %s   Build: %s   Run: %s\n", e.ItemPath, build, run)
	fmt.Fprintf(&b, "Event: %s\n", msg)
	if resolved {
		fmt.Fprintf(&b, "\n--- Tenant instructions (%s at %s) ---\n%s\n--- end of Tenant instructions ---\n", path, revision, body)
	}
	return b.String()
}

// buildOf reads the build number that the session context carries.
func buildOf(sess agent.Session) int64 {
	switch v := sess.Context["build"].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}
