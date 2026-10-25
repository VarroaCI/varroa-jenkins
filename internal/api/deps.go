package api

import (
	"log/slog"
	"time"

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
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// Dependencies holds the shared dependencies for API handlers.
type Dependencies struct {
	Client controller.ResourceClient
	Store  crdstore.Backend // generic CRD store
	// Controllers serves local Controller lists from the BFF informer cache.
	// Nil lists through Store. Use LocalControllers, not this field, to read.
	Controllers  ControllerLister
	CRClient     client.Client // controller-runtime client for read-only CRD operations
	Authorizer   *Authorizer
	MiteRegistry transport.Transport
	Auth         auth.Provider

	// JenkinsTokenSigner is used by the MCP proxy to mint per-caller Jenkins tokens; nil when the mite signing key is unavailable.
	JenkinsTokenSigner *mitesrv.MiteTokenSigner

	// Local is set in local auth mode and when local accounts run beside
	// OIDC; nil otherwise.
	Local *local.Provider
	// LDAP is set in ldap auth mode; nil otherwise.
	LDAP         *ldap.Provider
	KeyVerifier  *apikey.Verifier
	TicketIssuer *auth.TicketIssuer
	Broadcaster  sse.EventSource
	// RunFanout carries live run notifications to the tenant run stream. It is
	// nil in tests that do not exercise that stream.
	RunFanout         sse.RunEventSource
	LogBuffer         *logbuffer.LogBuffer
	Logger            *slog.Logger
	OperatorNamespace string
	ManagedNamespaces string // raw MANAGED_NAMESPACES value; "" ⇒ cluster-wide mode
	ActivityPublisher activity.EventSink
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

	// VersionProfileReconciler re-materializes a JenkinsVersionProfile's
	// derived plugin-set ConfigMap synchronously, in-request, during
	// candidate promotion. May be nil in tests that don't exercise
	// promotion.
	VersionProfileReconciler *controller.JenkinsVersionProfileReconciler

	// FleetLockReleaser asks the recorder to force-release a releasing
	// fleet lock. Nil when the record store is off, so the release route
	// answers 503.
	FleetLockReleaser FleetLockReleaser

	// RunNotePoster asks the recorder to write a run or job note. Nil when
	// the record store or the bus is off, so the note write answers 503.
	RunNotePoster RunNotePoster

	// TenantAgents is the cached view of the TenantAgent, Tenant, and CatalogSource
	// objects. Nil turns the Tenant agent tools off.
	TenantAgents TenantAgentSource
	// ForgeRelay sends forge checks, reads, pull requests, and issues to the
	// operator of a cluster. Nil turns the forge-backed routes and tools off.
	ForgeRelay ForgeRelay
	// ForgePermission reads the repository permission of a forge user for
	// the tenant door's comment check. Nil refuses every comment trigger.
	ForgePermission ForgePermissionChecker
	// WebhookEnqueuer queues a signed tenant webhook through the recorder.
	WebhookEnqueuer WebhookEnqueuer
	// CoreCluster names the core cluster, which answers every forge request.
	CoreCluster string

	// RecordStore reads build records the recorder wrote. Nil when
	// VARROA_RECORDSTORE_DSN is unset, so a handler on this surface must
	// check for nil before use.
	RecordStore recordstore.Reader

	// Assistant is the assistant runtime. Nil turns every assistant route off.
	Assistant *AssistantRuntime

	// RecordStoreRunRetention is the store-wide runs window, from
	// VARROA_RETENTION_RUNS. It caps every tenant and job window. Zero
	// means the 365d default.
	RecordStoreRunRetention time.Duration

	// BlobStore reads a run's console log and artifact bytes. Nil when
	// VARROA_BLOBSTORE_URL is unset, so a handler on the log or artifact
	// path must check for nil before use.
	BlobStore blob.Store

	// TenantResolver maps a pipelines {tenant} URL segment onto record-store
	// tenants. Nil in tests that do not exercise the pipelines surface.
	TenantResolver TenantResolver

	// RunnerTokenIndex resolves a runner door token to its Runner name
	// (design D7). Rebuilt from the BFF's own Runner watch. Nil in tests
	// that do not exercise the agent WebSocket door; the door answers 401
	// for every token when it is nil.
	RunnerTokenIndex *RunnerTokenIndex

	// CellMaxTenants is the chart's cells.maxTenantsPerCell value, from
	// VARROA_CELLS_MAX_TENANTS. It is the second tier of the effective
	// tenant cap the cell usage reads report. Zero when the variable is
	// missing or is not a whole number.
	CellMaxTenants int

	// JenkinsUpstream is the state of the Jenkins upstream metadata host,
	// from VARROA_JENKINS_UPSTREAM_URL. The zero value means the built-in
	// host, the same rule as an absent env var.
	JenkinsUpstream controller.JenkinsUpstreamState
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
	// LocalAccounts is true when declared local accounts sign in beside a
	// primary OIDC provider.
	LocalAccounts bool
}

// LocalAccountsOn reports whether local accounts exist on this release:
// local mode, or local accounts beside OIDC.
func (c IdentityConfig) LocalAccountsOn() bool {
	return c.Mode == string(auth.AuthModeLocal) || c.LocalAccounts
}
