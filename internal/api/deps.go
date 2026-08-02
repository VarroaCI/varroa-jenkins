package api

import (
	"log/slog"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/api/logbuffer"
	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	"github.com/varroaci/varroa-jenkins/internal/apikey"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/auth/ldap"
	"github.com/varroaci/varroa-jenkins/internal/auth/local"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// Dependencies holds the shared dependencies for API handlers.
type Dependencies struct {
	Client       controller.ResourceClient
	Store        crdstore.Backend // generic CRD store
	CRClient     client.Client    // controller-runtime client for read-only CRD operations
	Authorizer   *Authorizer
	MiteRegistry transport.Transport
	Auth         auth.Provider

	// JenkinsTokenSigner is used by the MCP proxy to mint per-caller Jenkins tokens; nil when the mite signing key is unavailable.
	JenkinsTokenSigner *mitesrv.MiteTokenSigner

	// Local is set in local auth mode; nil otherwise.
	Local *local.Provider
	// LDAP is set in ldap auth mode; nil otherwise.
	LDAP              *ldap.Provider
	KeyVerifier       *apikey.Verifier
	TicketIssuer      *auth.TicketIssuer
	Broadcaster       sse.EventSource
	LogBuffer         *logbuffer.LogBuffer
	Logger            *slog.Logger
	OperatorNamespace string
	ManagedNamespaces string // raw MANAGED_NAMESPACES value; "" ⇒ cluster-wide mode
	ActivityStore     *activity.Store
	ActivityPublisher *activity.Publisher
	Backfill          activity.Backfill
	Reconciler        controller.ReconcilerAPI
	IdentityConfig    IdentityConfig
	ObsNormalizer     *ObservatoryNormalizer // observability normalizer (may be nil)
	ObsBackends       BackendIntegrationProvider
	// DashboardHost is the hostname serving the dashboard; empty disables the path-mode host-equality check.
	DashboardHost string

	// DashboardURL is the externally-reachable dashboard origin, used for
	// post_logout_redirect_uri and the mite banner back-link.
	DashboardURL string

	// OIDCStateSecret is the HMAC-SHA256 signing key for the oidc_state cookie.
	// Must be at least 32 bytes in OIDC mode. Empty in non-OIDC modes.
	OIDCStateSecret []byte

	// SecureCookies controls whether cookies set Secure flag. False only for
	// explicitly validated HTTP local-development dashboard URLs.
	SecureCookies bool

	// Brood is the multi-cluster controller CRUD router (§4).
	// May be nil in tests that don't need multi-cluster operations.
	Brood Brood

	// BusConn is the NATS bus connection, used by the hibernation handler
	// to publish webhook payloads and wake commands.
	BusConn *bus.Conn

	// BroodOps is the cross-cluster brood operation fan-out component (§6).
	// May be nil in tests that don't need brood operation fan-out.
	BroodOps BroodOps

	// BroodSchedules is the per-cluster brood schedule CRUD component.
	// May be nil in tests that don't need brood schedule operations.
	BroodSchedules BroodSchedules

	// ConfigBrood is the multi-cluster config CRUD router (add-remote-config-authoring).
	// May be nil in tests that don't need config operations.
	ConfigBrood ConfigBrood

	// FleetPluginInventory reads the classified per-controller plugin inventory
	// from T2.1's invc/ read model. May be nil in tests or when the transport
	// does not yet carry the classified accessor.
	FleetPluginInventory FleetPluginInventory

	// UpdateCenterInventory provides access to the update center's plugin
	// inventory; may be nil in tests or when VARROA_UPDATE_CENTER_URL is unset.
	UpdateCenterInventory UpdateCenterInventory

	// UpdateCenterUploader relays an authenticated plugin upload to the update
	// center; may be nil in tests or when VARROA_UPDATE_CENTER_URL or
	// VARROA_UPDATE_CENTER_IMPORT_TOKEN is unset.
	UpdateCenterUploader UpdateCenterUploader
}

// IdentityConfig holds the identity/OIDC configuration surfaced to the admin
// identity-settings endpoint. The client secret is never included.
type IdentityConfig struct {
	Mode         string   // "oidc", "local", or "ldap"
	Issuer       string   // OIDC issuer URL
	ClientID     string   // OIDC client ID
	Scopes       []string // OIDC scopes
	CookieDomain string   // cookie domain for varroa_token
	DefaultRead  bool     // global default-read flag
}
