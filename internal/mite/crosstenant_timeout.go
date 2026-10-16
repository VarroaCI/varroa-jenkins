package mite

import (
	"os"
	"strconv"
	"time"
)

// crossTenantGatewayTimeoutEnv overrides the gateway's wait for the
// record store on a cross-tenant request (design.md D14).
const crossTenantGatewayTimeoutEnv = "VARROA_CROSSTENANT_GATEWAY_TIMEOUT_MS"

// defaultCrossTenantGatewayTimeout is VARROA_CROSSTENANT_GATEWAY_TIMEOUT_MS's
// default: the gateway's hop of the shrink-inward cross-tenant timeout
// budget (design.md D14).
const defaultCrossTenantGatewayTimeout = 8500 * time.Millisecond

// crossTenantMiteHopTimeout is the mite's own default budget for waiting
// on the gateway's cross-tenant reply (design.md D14,
// VARROA_CROSSTENANT_MITE_TIMEOUT_MS). The gateway's own budget must stay
// shorter, because the mite is the caller waiting on this hop; an
// operator raises an outer budget before an inner one.
const crossTenantMiteHopTimeout = 9 * time.Second

// CrossTenantTimeoutFromEnv reads envVar as a whole number of
// milliseconds and converts it to a time.Duration. An unset, empty,
// non-integer, or non-positive value keeps def unchanged. Every
// VARROA_CROSSTENANT_*_TIMEOUT_MS budget (design.md D14) shares this
// rule; cmd/mite's own hop reuses this function rather than a second
// copy of it.
func CrossTenantTimeoutFromEnv(envVar string, def time.Duration) time.Duration {
	v := os.Getenv(envVar)
	if v == "" {
		return def
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms <= 0 {
		return def
	}
	return time.Duration(ms) * time.Millisecond
}

// crossTenantGatewayTimeout resolves the gateway's own cross-tenant
// budget and warns once per call when it is at least as long as the
// mite's own hop default, because that would leave the mite's whole
// budget consumed before the gateway even gives up (design.md D14: an
// operator raises an outer budget before an inner one).
func (h *BusHandler) crossTenantGatewayTimeout() time.Duration {
	d := CrossTenantTimeoutFromEnv(crossTenantGatewayTimeoutEnv, defaultCrossTenantGatewayTimeout)
	if d >= crossTenantMiteHopTimeout {
		h.log().Warn(
			"VARROA_CROSSTENANT_GATEWAY_TIMEOUT_MS is at least as long as the mite hop's default budget; raise the mite's budget first",
			"gateway_timeout_ms", d.Milliseconds(),
			"mite_hop_default_ms", crossTenantMiteHopTimeout.Milliseconds(),
		)
	}
	return d
}
