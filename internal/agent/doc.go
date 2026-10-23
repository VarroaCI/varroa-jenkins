// Package agent holds the core types of the assistant and tenant agents: the
// session, event, approval, and settings records, the enumerations that the
// record store and the wire share, and the create decision.
//
// The package imports no MCP, API, or record-store package in non-test code.
// Its sub-packages hold the policy, data-sharing, budget, prompt, store,
// and provider logic.
package agent
