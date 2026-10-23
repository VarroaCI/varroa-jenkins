package sessions

import "github.com/prometheus/client_golang/prometheus"

// SessionsTotal counts the sessions that a replica ends, by their final state.
var SessionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "varroa_agent_sessions_total",
	Help: "Tenant agent sessions that this replica ran to an end, by final state.",
}, []string{"state"})

// ToolCalls counts the MCP tool calls of the Tenant agent sessions, by the class
// of the tool and the outcome of the call.
var ToolCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "varroa_agent_tool_calls_total",
	Help: "Tenant agent MCP tool calls, by tool class and outcome.",
}, []string{"class", "outcome"})
