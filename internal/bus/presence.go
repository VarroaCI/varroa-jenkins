package bus

import "time"

const (
	// PresenceTTL is the entry lifetime of the presence bucket. A record
	// that no gateway writes again is gone after it, so the record of a
	// gateway replica that stopped ages out. It must stay below
	// PresenceStaleAfter: the last record of a stopped gateway then reads
	// as missing, never as a silent open stream.
	PresenceTTL = 90 * time.Second

	// PresenceRefreshInterval is how often a gateway writes the presence
	// record of each open stream when no mite message wrote it. It must
	// stay well below PresenceTTL, so the record of an open stream never
	// ages out.
	PresenceRefreshInterval = 15 * time.Second

	// PresenceStaleAfter is the heartbeat age past which a mite counts as
	// silent. The operator, the dashboard back end, and the gateway warning
	// use this one value.
	PresenceStaleAfter = 2 * time.Minute

	// PresenceReadLoopRecv is the ReadLoop value of a gateway read loop
	// that waits for the next mite message. Every other value is the oneof
	// field name of the mite message whose handler blocks the loop.
	PresenceReadLoopRecv = "recv"
)

// Presence is the gateway-written liveness record for a connected mite.
// It carries metadata that the generic StateSnapshot proto cannot hold:
// the actual heartbeat timestamp (as observed by the gateway) and cert
// expiry from the mTLS handshake.
type Presence struct {
	// Version is the version of the mite binary, from the hello or the
	// last heartbeat of the mite. It is never the Jenkins version: the
	// state snapshot carries that.
	Version string `json:"version"`
	// LastHeartbeat is the time of the last mite message that refreshed
	// liveness, never the time of the write. A record of a silent stream
	// therefore keeps an old value.
	LastHeartbeat time.Time `json:"lastHeartbeat"`
	CertExpiry    time.Time `json:"certExpiry"`
	Epoch         int64     `json:"epoch"` // connection epoch, stable across heartbeats

	// StreamOpen reports that the gateway that wrote the record holds an
	// open stream of the mite. The gateway writes the record again every
	// PresenceRefreshInterval while the stream is open, also when no mite
	// message arrives. A record with StreamOpen and a stale LastHeartbeat
	// is a silent open stream, not a lost one.
	StreamOpen bool `json:"streamOpen"`

	// ReadLoop names what the gateway read loop of a silent stream does:
	// PresenceReadLoopRecv while it waits for a mite message, else the
	// message kind whose handler blocks it, for example "log_chunk". The
	// gateway sets it only while LastHeartbeat is older than
	// PresenceStaleAfter. The next heartbeat or snapshot clears it.
	ReadLoop string `json:"readLoop,omitempty"`
	// ReadLoopSince is the time at which the read loop entered ReadLoop.
	ReadLoopSince time.Time `json:"readLoopSince,omitzero"`

	// IdleGaugesJSON is the JSON-serialized idle gauges from the latest
	// heartbeat, or empty if none have been received.
	IdleGaugesJSON string `json:"idleGauges,omitempty"`

	// IdleGaugesReceivedAt is the time the idle gauges were received.
	IdleGaugesReceivedAt time.Time `json:"idleGaugesReceivedAt,omitempty"`

	// InstalledPluginsHash is the installed_plugins_hash from the mite's most
	// recent heartbeat. Empty if the mite has not sent one yet.
	InstalledPluginsHash string `json:"installedPluginsHash,omitempty"`

	// StreamDegraded reports that the gateway's bus→gRPC bridge for this mite
	// is broken: a subscription or JetStream consumer could not be established
	// (or died) and is being retried. The mite still holds a healthy gRPC
	// stream, so it looks connected — but operator-published desired state is
	// not reaching it. StreamDegradedReason carries the underlying error.
	StreamDegraded       bool   `json:"streamDegraded,omitempty"`
	StreamDegradedReason string `json:"streamDegradedReason,omitempty"`
}

// HeartbeatFresh reports whether the record shows a live mite at now: its
// last heartbeat is no older than PresenceStaleAfter.
func (p Presence) HeartbeatFresh(now time.Time) bool {
	return now.Sub(p.LastHeartbeat) <= PresenceStaleAfter
}
