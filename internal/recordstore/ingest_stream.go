package recordstore

import (
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// EnsureBuildsStream creates or updates the varroa_builds stream with
// design D3's configuration. cmd/recorder calls this on start, and no
// other component may create this stream.
func EnsureBuildsStream(conn *bus.Conn, replicas int) error {
	return conn.EnsureStream(bus.BuildsStreamConfig(bus.BuildsStreamName, replicas))
}
