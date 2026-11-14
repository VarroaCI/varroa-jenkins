package bus

// Assistant subjects. The subject scheme comment in subjects.go lists them.
//
// Agent session subjects:     agentsessions.<cluster>.<tenant> (core NATS, at-most-once)
// Assistant control subject:  varroa.assistant.control (core NATS, at-most-once)

const (
	// AgentSessionsWildcard captures every agent session notification. Each
	// message names the session and its state only. It carries no transcript.
	AgentSessionsWildcard = "agentsessions.>"

	// AgentSessionNoTenant is the tenant token of a session with no tenant.
	AgentSessionNoTenant = "_none"

	// AssistantControlSubject carries a notice of a kill switch write or an
	// unattended grant write. A varroa-agent replica wakes its status ticker on
	// it. No reader keeps a copy of the settings.
	AssistantControlSubject = "varroa.assistant.control"
)

// AgentSessionSubject returns the notification subject of a session. An
// empty tenant uses AgentSessionNoTenant.
func AgentSessionSubject(cluster, tenant string) string {
	if tenant == "" {
		tenant = AgentSessionNoTenant
	}
	return "agentsessions." + cluster + "." + tenant
}
