package bus

import "time"

// Presence is the gateway-written liveness record for a connected mite.
// It carries metadata that the generic StateSnapshot proto cannot hold:
// the actual heartbeat timestamp (as observed by the gateway) and cert
// expiry from the mTLS handshake.
type Presence struct {
	Version       string    `json:"version"`
	LastHeartbeat time.Time `json:"lastHeartbeat"`
	CertExpiry    time.Time `json:"certExpiry"`
	Epoch         int64     `json:"epoch"` // connection epoch, stable across heartbeats

	// IdleGaugesJSON is the JSON-serialized idle gauges from the latest
	// heartbeat, or empty if none have been received.
	IdleGaugesJSON string `json:"idleGauges,omitempty"`

	// IdleGaugesReceivedAt is the time the idle gauges were received.
	IdleGaugesReceivedAt time.Time `json:"idleGaugesReceivedAt,omitempty"`

	// InstalledPluginsHash is the installed_plugins_hash from the mite's most
	// recent heartbeat. Empty if the mite has not sent one yet.
	InstalledPluginsHash string `json:"installedPluginsHash,omitempty"`
}
