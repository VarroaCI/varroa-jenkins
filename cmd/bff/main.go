// Command bff is the Varroa Backend-For-Frontend tier. It serves the REST API
// and SSE event streams to the browser. It is stateless: all mite telemetry
// comes from the NATS bus, and OIDC JWT validation requires no server-side
// session. Multiple BFF replicas can run behind a load balancer.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/time/rate"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	"github.com/varroaci/varroa-jenkins/internal/apikey"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	ldappkg "github.com/varroaci/varroa-jenkins/internal/auth/ldap"
	localpkg "github.com/varroaci/varroa-jenkins/internal/auth/local"
	"github.com/varroaci/varroa-jenkins/internal/auth/schedule"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/logging"
	mcp "github.com/varroaci/varroa-jenkins/internal/mcp"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
	rbac "github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/signing"
	"github.com/varroaci/varroa-jenkins/internal/telemetry"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

var version = "dev"

func main() {
	port := flag.Int("port", 8080, "HTTP listen port")
	busURL := flag.String("bus-url", "", "NATS bus URL")
	busUser := flag.String("bus-user", "bff", "NATS bus username")
	busPassFile := flag.String("bus-pass-file", "", "Path to NATS bus password file (overrides BUS_PASSWORD env)")
	busCAFile := flag.String("bus-ca-file", "", "Path to NATS bus CA certificate file")
	oidcIssuer := flag.String("oidc-issuer", "", "OIDC issuer URL")
	oidcClientID := flag.String("oidc-client-id", "", "OIDC client ID")
	oidcClientSecret := flag.String("oidc-client-secret", "", "OIDC client secret")
	oidcRedirectURL := flag.String("oidc-redirect-url", "", "OIDC redirect URL")
	defaultRead := flag.Bool("default-read", false, "Allow any authenticated user to read controllers without explicit binding")
	oidcCookieDomain := flag.String("oidc-cookie-domain", envDefault("VARROA_COOKIE_DOMAIN", ""), "domain for varroa_token cookie (e.g. .example.com)")
	oidcScopes := flag.String("oidc-scopes", envDefault("VARROA_OIDC_SCOPES", "openid,profile,email,groups"), "comma-separated OIDC scopes")
	ldapURL := flag.String("ldap-url", envDefault("VARROA_LDAP_URL", ""), "LDAP server URL (ldap:// or ldaps://)")
	ldapBindDNTemplate := flag.String("ldap-bind-dn-template", envDefault("VARROA_LDAP_BIND_DN_TEMPLATE", ""), "LDAP direct-bind DN template (e.g. uid=%s,ou=people,dc=example,dc=com)")
	ldapUserSearchBase := flag.String("ldap-user-search-base", envDefault("VARROA_LDAP_USER_SEARCH_BASE", ""), "LDAP user search base DN")
	ldapUserSearchFilter := flag.String("ldap-user-search-filter", envDefault("VARROA_LDAP_USER_SEARCH_FILTER", "(uid=%s)"), "LDAP user search filter")
	ldapUserEmailAttr := flag.String("ldap-user-email-attr", envDefault("VARROA_LDAP_USER_EMAIL_ATTR", "mail"), "LDAP user email attribute")
	ldapUserNameAttr := flag.String("ldap-user-name-attr", envDefault("VARROA_LDAP_USER_NAME_ATTR", "cn"), "LDAP user display name attribute")
	ldapServiceAccountDN := flag.String("ldap-service-account-dn", envDefault("VARROA_LDAP_SERVICE_ACCOUNT_DN", ""), "LDAP service account DN for search-then-bind")
	ldapServiceAccountPassword := flag.String("ldap-service-account-password", envDefault("VARROA_LDAP_SERVICE_ACCOUNT_PASSWORD", ""), "LDAP service account password")
	ldapGroupSearchBase := flag.String("ldap-group-search-base", envDefault("VARROA_LDAP_GROUP_SEARCH_BASE", ""), "LDAP group search base DN")
	ldapGroupSearchFilter := flag.String("ldap-group-search-filter", envDefault("VARROA_LDAP_GROUP_SEARCH_FILTER", "(member=%s)"), "LDAP group search filter")
	ldapGroupNameAttr := flag.String("ldap-group-name-attr", envDefault("VARROA_LDAP_GROUP_NAME_ATTR", "cn"), "LDAP group name attribute")
	ldapStartTLS := flag.Bool("ldap-start-tls", false, "Enable STARTTLS on plain ldap:// connections")
	ldapInsecureSkipVerify := flag.Bool("ldap-insecure-skip-verify", false, "UNSAFE — disables TLS verification (testing only); prefer --ldap-ca-cert-file")
	ldapCACertFile := flag.String("ldap-ca-cert-file", envDefault("VARROA_LDAP_CA_CERT_FILE", ""), "Path to a PEM CA bundle used to verify the LDAP server certificate")
	kubeconfigPath := flag.String("kubeconfig", "", "Explicit kubeconfig path")
	authMode := flag.String("auth-mode", envDefault("VARROA_AUTH_MODE", "oidc"), "Auth mode: oidc, local, or ldap")
	bffURL := flag.String("bff-url", envDefault("VARROA_BFF_URL", ""), "BFF URL (required when auth-mode=local or ldap)")
	dashboardURL := flag.String("dashboard-url", envDefault("VARROA_DASHBOARD_URL", ""), "External dashboard URL (required in OIDC mode; absolute HTTPS origin)")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	logFormat := flag.String("log-format", "text", "Log format: text, json")
	flag.Parse()

	level, err := logging.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q: %v\n", *logLevel, err)
		os.Exit(1)
	}
	logger := logging.New(level, *logFormat, os.Stderr).With("binary", "bff")
	slog.SetDefault(logger)

	cluster, err := bus.ClusterFromEnv()
	if err != nil {
		logger.Error("invalid cluster name", "error", err)
		os.Exit(1)
	}
	logger.Info("cluster identity", "cluster", cluster)

	_ = os.Setenv("VARROA_SERVICE_NAME", "varroa-bff")
	telemetryShutdown := telemetry.InitTelemetry(context.Background())
	if !telemetry.Disabled() {
		logger = telemetry.LogHandler(logger)
		slog.SetDefault(logger)
	}
	telemetryDisabled := telemetry.Disabled()
	defer func() {
		if telemetryShutdown != nil {
			if err := telemetryShutdown(context.Background()); err != nil {
				logger.Warn("telemetry shutdown failed", "error", err)
			}
		}
	}()
	_ = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") // consumed at route registration

	// Validate dashboard URL.
	if *dashboardURL != "" {
		du, err := url.Parse(*dashboardURL)
		if err != nil || du.Scheme == "" || du.Host == "" || du.Path != "" || (du.Scheme != "https" && du.Scheme != "http") {
			logger.Error("invalid --dashboard-url: must be an absolute origin (scheme://host) without a path; use HTTPS in production", "url", *dashboardURL)
			os.Exit(1)
		}
		if du.Scheme != "https" && !strings.HasPrefix(du.Host, "localhost") && !strings.HasSuffix(du.Host, ".localtest.me") {
			logger.Error("insecure --dashboard-url: non-HTTPS origin is only allowed for local development (localhost or *.localtest.me)", "url", *dashboardURL)
			os.Exit(1)
		}
	} else if *authMode == "oidc" {
		logger.Error("--dashboard-url is required in OIDC auth mode")
		os.Exit(1)
	}

	operatorNamespace := controller.ResolveOperatorNamespace(logger)
	managedNamespaces := os.Getenv("MANAGED_NAMESPACES")

	// --- Kubernetes client (for CRUD operations on CRDs) ---
	var clientsetClient *controller.ClientsetClient
	var client controller.ResourceClient
	if *kubeconfigPath != "" {
		var err error
		clientsetClient, err = controller.NewClientsetClientWithKubeconfig(*kubeconfigPath)
		if err != nil {
			logger.Error("failed to create clientset client (kubeconfig)", "error", err)
			os.Exit(1)
		}
	} else {
		var err error
		clientsetClient, err = controller.NewClientsetClient()
		if err != nil {
			logger.Error("failed to create clientset client", "error", err)
			os.Exit(1)
		}
	}
	client = clientsetClient

	// --- Resolve OIDC flags from env vars if not set ---
	if *oidcIssuer == "" {
		*oidcIssuer = os.Getenv("VARROA_OIDC_ISSUER")
	}
	if *oidcClientID == "" {
		*oidcClientID = os.Getenv("VARROA_OIDC_CLIENT_ID")
	}
	if *oidcClientID == schedule.Audience {
		logger.Error("oidc-client-id must not equal the reserved schedule audience", "value", *oidcClientID)
		os.Exit(1)
	}
	if *oidcClientSecret == "" {
		*oidcClientSecret = os.Getenv("VARROA_OIDC_CLIENT_SECRET")
	}
	if *oidcRedirectURL == "" {
		*oidcRedirectURL = os.Getenv("VARROA_OIDC_REDIRECT_URL")
	}

	// --- Bus connection ---
	if *busURL == "" {
		*busURL = os.Getenv("VARROA_BUS_URL")
	}

	// Bus credentials: the file path is the production form because it is
	// re-read on every reconnect and so follows a rotated Secret; the env var
	// is the fallback for local runs with no mounted file.
	busCfg := bus.Config{
		Username:    *busUser,
		CAFile:      *busCAFile,
		InboxPrefix: "_INBOX_bff",
		Logger:      logger,
	}
	if *busPassFile != "" {
		busCfg.PasswordFile = *busPassFile
	} else {
		busCfg.Password = os.Getenv("BUS_PASSWORD")
	}
	busConn, err := bus.Connect(*busURL, busCfg)
	if err != nil {
		logger.Error("failed to connect to bus", "error", err)
		os.Exit(1)
	}
	defer busConn.Close()
	if err := busConn.RegisterMetrics(otel.Meter("varroa-bff"), "bff"); err != nil {
		logger.Warn("failed to register bus connected gauge", "error", err)
	}
	// JetStream replica count for streams and KV buckets (from the NATS cluster
	// size, clamped 1..3 by the chart). Even hive-mode BFFs read the core's
	// replica count via this env. Values < 1 are clamped to 1 by SetReplicas.
	busConn.SetReplicas(jetStreamReplicasFromEnv())
	logger.Info("connected to NATS bus", "url", busConn.NATSConn().ConnectedUrl(), "jetStreamReplicas", busConn.Replicas())

	// --- Mite read model (KV-backed, reads from gateway; read-only, no inbound subscriptions) ---
	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 5*time.Minute)
	if err != nil {
		logger.Error("failed to create snapshot KV", "error", err)
		os.Exit(1)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, 90*time.Second)
	if err != nil {
		logger.Error("failed to create presence KV", "error", err)
		os.Exit(1)
	}
	clustersKV, err := busConn.EnsureKV(bus.KVClustersBucket, bus.ClusterEntryTTL)
	if err != nil {
		logger.Error("failed to create clusters KV", "error", err)
		os.Exit(1)
	}
	clusterDir := bus.NewClusterDirectory(clustersKV)
	readModel := transport.NewBusReadModel(cluster, busConn, snapshotKV, presenceKV)
	miteTransport := transport.NewBusReadModelTransport(readModel)

	// Load the shared mite signing key for local auth mode user JWT issuance.
	// Token refresh grants are operator-owned in bus mode; the BFF does not
	// respond to mite TokenRefreshRequest messages.
	// The operator creates this Secret; the BFF reads it to share the same KID.
	var tokenSigner *mitesrv.MiteTokenSigner
	{
		deadline := time.Now().Add(60 * time.Second)
		for {
			keyData, err := client.GetSecret(context.Background(), "varroa-mite-signing-key", operatorNamespace)
			if err == nil && len(keyData["private-key"]) > 0 {
				tokenSigner, err = mitesrv.NewMiteTokenSignerFromPEM(keyData["private-key"])
				if err != nil {
					logger.Error("failed to load mite signing key", "error", err)
					os.Exit(1)
				}
				logger.Info("loaded mite signing key", "kid", tokenSigner.KID())
				break
			}
			if time.Now().After(deadline) {
				logger.Warn("mite signing key not available, token refresh will not work")
				break
			}
			time.Sleep(2 * time.Second)
		}
	}
	if tokenSigner != nil {
		logger.Info("loaded mite signing key for local auth", "kid", tokenSigner.KID())
	}

	// --- SSE bus fanout (replaces Broadcaster) ---
	fanout := sse.NewBusFanout(busConn)
	fanout.Logger = logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := fanout.Start(ctx); err != nil {
			logger.Error("sse fanout error", "error", err)
		}
	}()

	// --- Cluster membership gauge (15s ticker) ---
	go func() {
		meter := otel.Meter("varroa-bff")
		gauge, _ := meter.Int64Gauge("varroa.clusters.known",
			metric.WithDescription("Member clusters currently present in varroa_clusters"))
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if cs, err := clusterDir.List(); err == nil {
					gauge.Record(ctx, int64(len(cs)))
				}
			}
		}
	}()

	// --- Activity stream lifecycle + persistence ---
	// Read retention config from environment.
	retentionStr := envDefault("VARROA_ACTIVITY_RETENTION", "7d")
	maxMsgsStr := envDefault("VARROA_ACTIVITY_MAX_MSGS", "100000")
	maxBytesStr := envDefault("VARROA_ACTIVITY_MAX_BYTES", "1073741824")

	maxAge, off, fallback := activity.ParseRetention(retentionStr)
	if fallback {
		logger.Warn("unrecognized VARROA_ACTIVITY_RETENTION value, falling back to 7d",
			"value", retentionStr)
	}

	var maxMsgs, maxBytes int64
	if _, err := fmt.Sscanf(maxMsgsStr, "%d", &maxMsgs); err != nil {
		logger.Warn("invalid VARROA_ACTIVITY_MAX_MSGS, using default 100000", "value", maxMsgsStr)
		maxMsgs = 100000
	}
	if _, err := fmt.Sscanf(maxBytesStr, "%d", &maxBytes); err != nil {
		logger.Warn("invalid VARROA_ACTIVITY_MAX_BYTES, using default 1073741824", "value", maxBytesStr)
		maxBytes = 1073741824
	}

	actStore := activity.New(200)
	actStore.Logger = logger

	var backfill activity.Backfill
	if off {
		logger.Info("activity retention is off; using in-memory ring with activity.> subscriber")

		// Single ring-writer: subscribe to activity.> and append to the ring.
		// This is the ONLY writer to the ring; user-action Notify sites route
		// through the Publisher (above) and the bus echoes back here.
		go func() {
			sub, err := busConn.Subscribe(bus.ActivityWildcard, func(msg *nats.Msg) {
				var e activity.Event
				if err := json.Unmarshal(msg.Data, &e); err != nil {
					return
				}
				actStore.Append(e)
			})
			if err != nil {
				logger.Error("subscribe activity.> failed", "error", err)
				return
			}
			defer func() { _ = sub.Unsubscribe() }()
			<-ctx.Done()
		}()

		backfill = activity.NewRingBackfill(actStore)
	} else {
		logger.Info("ensuring activity JetStream stream",
			"maxAge", maxAge, "maxMsgs", maxMsgs, "maxBytes", maxBytes)
		if err := busConn.EnsureStream(bus.ActivityStreamConfig(
			bus.ActivityStreamName, maxAge, maxMsgs, maxBytes, busConn.Replicas(),
		)); err != nil {
			logger.Error("failed to ensure activity stream", "error", err)
			os.Exit(1)
		}
		// Ensure the webhook replay stream (idempotent).
		if err := busConn.EnsureStream(bus.WebhookStreamConfig(
			bus.WebhookStreamName, busConn.Replicas(),
		)); err != nil {
			logger.Error("failed to ensure webhook stream", "error", err)
			os.Exit(1)
		}
		backfill = activity.NewJetStreamBackfill(cluster, busConn, bus.ActivityStreamName, 5000, int(maxAge.Hours()/24))
	}

	// Construct the activity Publisher (always present; publishes to bus regardless of mode).
	actPub := activity.NewPublisher(cluster, busConn)
	actPub.Logger = logger
	// Jenkins-source events are published by the gateway to the per-controller
	// activity.<ns>.<controller> subjects (see internal/mite/bus_handler.go), so
	// they are ingested by the activity.> ring subscriber (off mode) and the
	// JetStream stream (retention mode) like every other activity event — no
	// dedicated events.activity subscriber is needed here.

	// --- RBAC informers and authorizer ---
	dynamicClient := clientsetClient.DynamicClient()

	roleGVR := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "varroaroles"}
	roleInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return dynamicClient.Resource(roleGVR).List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return dynamicClient.Resource(roleGVR).Watch(context.TODO(), options)
			},
		},
		&unstructured.Unstructured{},
		0,
		cache.Indexers{},
	)
	if err := roleInformer.SetTransform(func(obj interface{}) (interface{}, error) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return obj, nil
		}
		role := &v1alpha1.VarroaRole{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, role); err != nil {
			return nil, err
		}
		return role, nil
	}); err != nil {
		logger.Error("failed to set transform on role informer", "error", err)
		os.Exit(1)
	}

	roleBindingGVR := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "varroarolebindings"}
	roleBindingInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return dynamicClient.Resource(roleBindingGVR).List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return dynamicClient.Resource(roleBindingGVR).Watch(context.TODO(), options)
			},
		},
		&unstructured.Unstructured{},
		0,
		cache.Indexers{rbac.BySubjectIndex: rbac.SubjectIndexFunc},
	)
	if err := roleBindingInformer.SetTransform(func(obj interface{}) (interface{}, error) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return obj, nil
		}
		binding := &v1alpha1.VarroaRoleBinding{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, binding); err != nil {
			return nil, err
		}
		return binding, nil
	}); err != nil {
		logger.Error("failed to set transform on role binding informer", "error", err)
		os.Exit(1)
	}

	// JenkinsRole informer
	jenkinsRoleGVR := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "jenkinsroles"}
	jenkinsRoleInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return dynamicClient.Resource(jenkinsRoleGVR).List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return dynamicClient.Resource(jenkinsRoleGVR).Watch(context.TODO(), options)
			},
		},
		&unstructured.Unstructured{}, 0, cache.Indexers{},
	)
	if err := jenkinsRoleInformer.SetTransform(func(obj interface{}) (interface{}, error) {
		u, _ := obj.(*unstructured.Unstructured)
		if u == nil {
			return obj, nil
		}
		jr := &v1alpha1.JenkinsRole{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, jr); err != nil {
			return nil, err
		}
		return jr, nil
	}); err != nil {
		logger.Error("failed to set transform on jenkins role informer", "error", err)
		os.Exit(1)
	}

	// JenkinsRoleBinding informer
	jenkinsRoleBindingGVR := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "jenkinsrolebindings"}
	jenkinsRoleBindingInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return dynamicClient.Resource(jenkinsRoleBindingGVR).List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return dynamicClient.Resource(jenkinsRoleBindingGVR).Watch(context.TODO(), options)
			},
		},
		&unstructured.Unstructured{}, 0,
		cache.Indexers{rbac.JenkinsBySubjectIndex: rbac.JenkinsSubjectIndexFunc},
	)
	if err := jenkinsRoleBindingInformer.SetTransform(func(obj interface{}) (interface{}, error) {
		u, _ := obj.(*unstructured.Unstructured)
		if u == nil {
			return obj, nil
		}
		binding := &v1alpha1.JenkinsRoleBinding{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, binding); err != nil {
			return nil, err
		}
		return binding, nil
	}); err != nil {
		logger.Error("failed to set transform on jenkins role binding informer", "error", err)
		os.Exit(1)
	}

	controllerGVR := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "controllers"}
	controllerInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return dynamicClient.Resource(controllerGVR).List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return dynamicClient.Resource(controllerGVR).Watch(context.TODO(), options)
			},
		},
		&unstructured.Unstructured{},
		0,
		cache.Indexers{},
	)
	if err := controllerInformer.SetTransform(func(obj interface{}) (interface{}, error) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return obj, nil
		}
		ctrl := &v1alpha1.Controller{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, ctrl); err != nil {
			return nil, err
		}
		return ctrl, nil
	}); err != nil {
		logger.Error("failed to set transform on controller informer", "error", err)
		os.Exit(1)
	}

	userClaims := strings.Split(envDefault("VARROA_OIDC_USER_CLAIM", "preferred_username,sub"), ",")
	groupClaims := strings.Split(envDefault("VARROA_OIDC_GROUP_CLAIM", "groups"), ",")
	rbacResolver := rbac.NewResolver(roleInformer, roleBindingInformer, jenkinsRoleInformer, jenkinsRoleBindingInformer, controllerInformer, *defaultRead,
		userClaims, groupClaims)
	authorizer := api.NewAuthorizer(rbacResolver, *defaultRead)
	configBrood := api.NewBusConfigBrood(cluster, busConn, client, clientsetClient, logger)
	rbacFederation := api.NewRBACFederationReconciler(
		roleInformer.GetStore(),
		roleBindingInformer.GetStore(),
		jenkinsRoleInformer.GetStore(),
		configBrood,
		clustersKV,
		cluster,
		logger,
	)
	registerRBACFederationHandlers(roleInformer, roleBindingInformer, rbacFederation, logger)

	// Start informers and wait for cache sync.
	go roleInformer.Run(ctx.Done())
	go roleBindingInformer.Run(ctx.Done())
	go controllerInformer.Run(ctx.Done())
	go jenkinsRoleInformer.Run(ctx.Done())
	go jenkinsRoleBindingInformer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		roleInformer.HasSynced,
		roleBindingInformer.HasSynced,
		controllerInformer.HasSynced,
		jenkinsRoleInformer.HasSynced,
		jenkinsRoleBindingInformer.HasSynced,
	) {
		logger.Error("failed to sync RBAC informer caches")
		os.Exit(1)
	}
	go runRBACFederationLeaderElection(ctx, clientsetClient.Clientset(), operatorNamespace, rbacFederation, logger)

	// --- API dependencies ---
	reconcilerProxy := controller.NewNATSReconcilerProxy(busConn)
	reconcilerProxy.SetLogger(logger)

	// Build a bundle resolver for controller reconcile operations.
	bundleResolver := bundle.NewResolver("/tmp/varroa-bundles")
	bundleResolver.Logger = logger
	if *oidcIssuer != "" {
		bundleResolver.SetOIDCConfig(*oidcIssuer, *oidcClientID, *oidcClientSecret)
	}
	// Derive dashboard host for path-mode host-equality check.
	dashboardHost := ""
	if (*authMode == "local" || *authMode == "ldap") && *bffURL != "" {
		if u, err := url.Parse(*bffURL); err == nil {
			dashboardHost = u.Hostname()
		}
	} else if *oidcRedirectURL != "" {
		if u, err := url.Parse(*oidcRedirectURL); err == nil {
			dashboardHost = u.Hostname()
		}
	}

	// Determine whether cookies should use Secure flag.
	// Only disable for explicitly validated HTTP local-development URLs.
	secureCookies := true
	if du, err := url.Parse(*dashboardURL); err == nil && du.Scheme == "http" {
		secureCookies = false
	}

	apiDeps := &api.Dependencies{
		Authorizer:        authorizer,
		Client:            client,
		Store:             clientsetClient, // *ClientsetClient implements crdstore.Backend
		MiteRegistry:      miteTransport,
		Broadcaster:       fanout, // bus-backed fanout replaces the in-process Broadcaster
		ActivityStore:     actStore,
		ActivityPublisher: actPub,
		Backfill:          backfill,
		OperatorNamespace: operatorNamespace,
		ManagedNamespaces: managedNamespaces,
		Logger:            logger,
		Reconciler:        reconcilerProxy,
		DashboardHost:     dashboardHost,
		DashboardURL:      *dashboardURL,
		SecureCookies:     secureCookies,
		Brood:             api.NewBusBrood(cluster, busConn, clusterDir, client, clientsetClient, logger),
		BusConn:           busConn,
		BroodOps:          api.NewBusBroodOps(cluster, busConn, clusterDir, client, clientsetClient, logger),
		BroodSchedules:    api.NewBusBroodSchedules(cluster, busConn, clusterDir, logger),
		ConfigBrood:       configBrood,
	}
	// Fleet plugin inventory reader: the classified accessor T2.1 publishes
	// through the transport. Always wired when the transport exists.
	apiDeps.FleetPluginInventory = api.NewFleetInventoryReader(miteTransport)
	if ucURL := os.Getenv("VARROA_UPDATE_CENTER_URL"); ucURL != "" {
		apiDeps.UpdateCenterInventory = api.NewUpdateCenterInventoryClient(ucURL, &http.Client{Timeout: 10 * time.Second})
		// The BFF is the only component that authenticates a real user, so it is
		// the only one that can attribute an upload — hence it, and not varroactl,
		// holds the update center's import token. A generous timeout: the upload
		// carries up to 256 MiB and the update center may fetch dependencies
		// before responding.
		if token := os.Getenv("VARROA_UPDATE_CENTER_IMPORT_TOKEN"); token != "" {
			apiDeps.UpdateCenterUploader = api.NewUpdateCenterUploadClient(ucURL, token, &http.Client{Timeout: 15 * time.Minute})
		} else {
			logger.Warn("VARROA_UPDATE_CENTER_IMPORT_TOKEN not set — plugin uploads will return update-center-unreachable")
		}
	}
	apiDeps.JenkinsTokenSigner = tokenSigner

	// Create a controller-runtime client for read-only CRD operations
	// (e.g., ResolveTargets preview). Uses the same REST config as the
	// dynamic client above.
	{
		crScheme := runtime.NewScheme()
		_ = clientgoscheme.AddToScheme(crScheme)
		_ = v1alpha1.AddToScheme(crScheme)
		crClient, crErr := crclient.New(clientsetClient.RESTConfig(), crclient.Options{Scheme: crScheme})
		if crErr != nil {
			logger.Error("failed to create controller-runtime client", "error", crErr)
			os.Exit(1)
		}
		apiDeps.CRClient = crClient
	}
	apiDeps.ObsNormalizer = api.NewObservatoryNormalizer(apiDeps.MiteRegistry, apiDeps.ObsBackends)
	apiDeps.VersionProfileReconciler = controller.NewJenkinsVersionProfileReconciler(client, clientsetClient, operatorNamespace, logger)
	apiServer := api.NewServer(apiDeps)

	// --- API key verifier (long-lived credentials) ---
	keyVerifier := apikey.NewVerifier(bffKeyStore{clientsetClient, clientsetClient}, operatorNamespace)
	apiDeps.KeyVerifier = keyVerifier

	// --- HTTP routes ---
	mux := http.NewServeMux()
	mux.Handle("/metrics", telemetry.MetricsAuthMiddleware(promhttp.Handler()))
	mux.Handle("/healthz", telemetry.HealthzHandlerWithBus(busConn.Connected))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "ok")
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"component": "bff", "version": version})
	})
	// MCP endpoint for AI agent tool discovery and invocation.
	// Registered before /api/v1/ to take priority as a more specific pattern.
	mux.Handle("/api/v1/mcp", mcp.NewHandler(apiDeps))
	// OIDC auth endpoints (registered before /api/v1/ catch-all for priority).
	mux.HandleFunc("/api/v1/auth/login", apiServer.HandleOIDCLogin)
	mux.HandleFunc("/api/v1/callback", apiServer.HandleOIDCCallback)
	otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if !telemetry.Disabled() && otelEndpoint != "" {
		var fanoutCh = make(chan []byte, 256)
		go func() {
			for range fanoutCh {
				continue // TODO: deserialize OTLP-HTTP, forward via otlptracegrpc
			}
		}()
		ipLimiter := &ipRateLimiter{limiters: make(map[string]*rateLimiterEntry)}
		mux.Handle("/api/v1/otel/v1/traces", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !ipLimiter.allow(parseClientIP(r)) {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			select {
			case fanoutCh <- body:
			default:
			}
			w.WriteHeader(http.StatusOK)
		}))
	}
	mux.Handle("/api/v1/", api.NewRouter(apiDeps))
	mux.HandleFunc("/varroa-banner.js", apiServer.HandleVarroaBanner)
	// Hibernation wake surface at the host root (unauthenticated, token-guarded;
	// AuthMiddleware passes through non-/api/ paths).
	mux.HandleFunc("/hibernation/", apiServer.HandleHibernationDispatch)

	// --- Auth provider construction ---
	var provider auth.Provider
	var signer *signing.Signer
	if tokenSigner != nil {
		signer = tokenSigner.Signer()
	}
	var scheduleVerifier auth.ScheduleVerifier
	if signer != nil {
		scheduleVerifier = schedule.NewVerifier(signer)
	}
	switch *authMode {
	case "local":
		if *bffURL == "" {
			logger.Error("--bff-url is required in local auth mode")
			os.Exit(1)
		}
		if signer == nil {
			logger.Error("signing key not available for local auth; ensure the operator Secret exists")
			os.Exit(1)
		}
		lp := localpkg.New(signer, bffUserStore{clientsetClient, client}, operatorNamespace, *bffURL, *oidcClientID, 8*time.Hour, *oidcCookieDomain)
		apiDeps.Local = lp
		provider = lp
		logger.Info("local auth provider initialized", "bffURL", *bffURL, "kid", signer.KID())

		// Mount OIDC discovery endpoints.
		if cfg, jwks, ok2 := provider.Discovery(); ok2 {
			mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write(cfg)
			})
			mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write(jwks)
			})
		}

	case "ldap":
		if *bffURL == "" {
			logger.Error("--bff-url is required in ldap auth mode")
			os.Exit(1)
		}
		if signer == nil {
			logger.Error("signing key not available for ldap auth; ensure the operator Secret exists")
			os.Exit(1)
		}
		if *ldapURL == "" {
			logger.Error("--ldap-url is required in ldap auth mode")
			os.Exit(1)
		}
		if *ldapCACertFile != "" && *ldapInsecureSkipVerify {
			logger.Error("--ldap-ca-cert-file and --ldap-insecure-skip-verify are mutually exclusive (a CA means 'verify against this')")
			os.Exit(1)
		}
		var ldapCACert []byte
		if *ldapCACertFile != "" {
			b, err := os.ReadFile(*ldapCACertFile)
			if err != nil {
				logger.Error("failed to read --ldap-ca-cert-file", "path", *ldapCACertFile, "error", err)
				os.Exit(1)
			}
			ldapCACert = b
		}

		cfg := ldappkg.Config{
			URL:                    *ldapURL,
			BindDNTemplate:         *ldapBindDNTemplate,
			ServiceAccountDN:       *ldapServiceAccountDN,
			ServiceAccountPassword: *ldapServiceAccountPassword,
			StartTLS:               *ldapStartTLS,
			InsecureSkipVerify:     *ldapInsecureSkipVerify,
			CACert:                 ldapCACert,
		}
		if *ldapUserSearchBase != "" {
			cfg.UserSearch = &ldappkg.UserSearchConfig{
				BaseDN:    *ldapUserSearchBase,
				Filter:    *ldapUserSearchFilter,
				EmailAttr: *ldapUserEmailAttr,
				NameAttr:  *ldapUserNameAttr,
			}
		}
		if *ldapGroupSearchBase != "" {
			cfg.GroupSearch = &ldappkg.GroupSearchConfig{
				BaseDN:   *ldapGroupSearchBase,
				Filter:   *ldapGroupSearchFilter,
				NameAttr: *ldapGroupNameAttr,
			}
		}
		lp := ldappkg.New(signer, cfg, *bffURL, *oidcClientID, 8*time.Hour, *oidcCookieDomain)
		apiDeps.LDAP = lp
		provider = lp
		logger.Info("ldap auth provider initialized", "bffURL", *bffURL, "kid", signer.KID(), "ldapURL", *ldapURL)

		// Mount OIDC discovery endpoints.
		if cfg2, jwks, ok2 := provider.Discovery(); ok2 {
			mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write(cfg2)
			})
			mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write(jwks)
			})
		}

	default: // oidc
		if *oidcIssuer != "" {
			stateSecretStr := os.Getenv("OIDC_STATE_SECRET")
			if len(stateSecretStr) < 32 {
				logger.Error("OIDC_STATE_SECRET must be at least 32 bytes in OIDC mode", "length", len(stateSecretStr))
				os.Exit(1)
			}
			apiDeps.OIDCStateSecret = []byte(stateSecretStr)

			validator, err := auth.NewValidator(context.Background(), *oidcIssuer, *oidcClientID, *oidcClientSecret, *oidcRedirectURL, *oidcScopes)
			if err != nil {
				logger.Error("failed to create OIDC validator", "error", err)
				os.Exit(1)
			}
			validator.SetCookieDomain(*oidcCookieDomain)
			provider = validator

			logger.Info("OIDC auth enabled", "issuer", *oidcIssuer, "endSessionSupported", validator.EndSessionEndpoint() != "")
		}
	}

	apiDeps.Auth = provider

	// Populate identity configuration for the identity-settings endpoint.
	apiDeps.IdentityConfig = api.IdentityConfig{
		Mode:         *authMode,
		Issuer:       *oidcIssuer,
		ClientID:     *oidcClientID,
		Scopes:       parseScopes(*oidcScopes),
		CookieDomain: *oidcCookieDomain,
		DefaultRead:  *defaultRead,
	}

	// Stream-ticket issuer/verifier for header-less SSE auth. Requires the local RS256
	// signer (present in local/ldap modes); in OIDC mode there is no BFF signer, so SSE
	// falls back to cookie auth and no tickets are minted.
	var ticketVerifier *auth.TicketVerifier
	if signer != nil {
		apiDeps.TicketIssuer = auth.NewTicketIssuer(signer, *bffURL, 30*time.Second)
		ticketVerifier = auth.NewTicketVerifier(signer.PublicKey(), *bffURL)
	}

	var authHandler http.Handler
	if !telemetryDisabled {
		otelHandler := otelhttp.NewHandler(mux, "varroa-bff")
		if provider != nil {
			authHandler = auth.AuthMiddleware(provider, keyVerifier, ticketVerifier, scheduleVerifier, otelHandler, logger)
		} else {
			authHandler = otelHandler
		}
	} else {
		if provider != nil {
			authHandler = auth.AuthMiddleware(provider, keyVerifier, ticketVerifier, scheduleVerifier, mux, logger)
		} else {
			authHandler = mux
		}
	}

	// --- Start HTTP ---
	addr := fmt.Sprintf(":%d", *port)
	srv := &http.Server{
		Addr:    addr,
		Handler: authHandler,
	}
	go func() {
		logger.Info("BFF listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	// --- Wait for shutdown ---
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Info("BFF shutting down...")
	cancel()
	_ = srv.Shutdown(context.Background())
}

// parseScopes splits a comma-separated scope string, trimming whitespace.
func parseScopes(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, s := range parts {
		s = strings.TrimSpace(s)
		if s != "" {
			result = append(result, s)
		}
	}
	return result
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// jetStreamReplicasFromEnv reads VARROA_JETSTREAM_REPLICAS, defaulting to 1
// when unset or unparseable. Values < 1 are treated as 1 (SetReplicas clamps).
func jetStreamReplicasFromEnv() int {
	v := os.Getenv("VARROA_JETSTREAM_REPLICAS")
	if v == "" {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 1
	}
	return n
}

func registerRBACFederationHandlers(roleInformer, bindingInformer cache.SharedIndexInformer, reconciler *api.RBACFederationReconciler, logger *slog.Logger) {
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { reconciler.Enqueue() },
		UpdateFunc: func(oldObj, newObj interface{}) { reconciler.Enqueue() },
		DeleteFunc: func(obj interface{}) { reconciler.Enqueue() },
	}
	if _, err := roleInformer.AddEventHandler(handler); err != nil {
		logger.Warn("failed to register VarroaRole federation handler", "error", err)
	}
	if _, err := bindingInformer.AddEventHandler(handler); err != nil {
		logger.Warn("failed to register VarroaRoleBinding federation handler", "error", err)
	}
}

func runRBACFederationLeaderElection(ctx context.Context, clientset kubernetes.Interface, namespace string, reconciler *api.RBACFederationReconciler, logger *slog.Logger) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown"
	}
	identity := fmt.Sprintf("%s-%d", hostname, os.Getpid())
	lock, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		namespace,
		"varroa-rbac-federation",
		clientset.CoreV1(),
		clientset.CoordinationV1(),
		resourcelock.ResourceLockConfig{Identity: identity},
	)
	if err != nil {
		logger.Error("failed to create RBAC federation lease lock", "error", err)
		return
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   30 * time.Second,
		RenewDeadline:   20 * time.Second,
		RetryPeriod:     5 * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				logger.Info("started RBAC federation leader loop", "lease", "varroa-rbac-federation", "identity", identity)
				reconciler.Run(leaderCtx)
			},
			OnStoppedLeading: func() {
				logger.Warn("stopped RBAC federation leader loop", "lease", "varroa-rbac-federation", "identity", identity)
			},
		},
	})
}

var _ = apierrors.IsNotFound // keep apierrors import

type ipRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rateLimiterEntry
}

type rateLimiterEntry struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

func (irl *ipRateLimiter) allow(ip string) bool {
	irl.mu.Lock()
	entry, ok := irl.limiters[ip]
	if !ok {
		if len(irl.limiters) > 10000 {
			irl.mu.Unlock()
			return false // drop under attack
		}
		entry = &rateLimiterEntry{limiter: rate.NewLimiter(100, 200), lastUsed: time.Now()}
		irl.limiters[ip] = entry
	}
	entry.lastUsed = time.Now()
	irl.mu.Unlock()
	return entry.limiter.Allow()
}

func parseClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// bffKeyStore adapts *ClientsetClient + crdstore.Backend to apikey.keyStore.
type bffKeyStore struct {
	client *controller.ClientsetClient
	store  crdstore.Backend
}

func (s bffKeyStore) GetUserCRD(ctx context.Context, name, namespace string) (*v1alpha1.User, error) {
	return crdstore.Get[v1alpha1.User](ctx, s.store, name, namespace)
}
func (s bffKeyStore) GetSecret(ctx context.Context, name, namespace string) (map[string][]byte, error) {
	return s.client.GetSecret(ctx, name, namespace)
}
func (s bffKeyStore) CreateSecretExclusive(ctx context.Context, name, namespace string, labels map[string]string, data map[string][]byte) error {
	return s.client.CreateSecretExclusive(ctx, name, namespace, labels, data)
}
func (s bffKeyStore) PatchSecretData(ctx context.Context, name, namespace string, data map[string][]byte) error {
	return s.client.PatchSecretData(ctx, name, namespace, data)
}
func (s bffKeyStore) DeleteSecret(ctx context.Context, name, namespace string) error {
	return s.client.DeleteSecret(ctx, name, namespace)
}
func (s bffKeyStore) ListSecrets(ctx context.Context, namespace, labelSelector string) ([]map[string][]byte, error) {
	return s.client.ListSecrets(ctx, namespace, labelSelector)
}
func (s bffKeyStore) ListGroupCRDs(ctx context.Context) ([]*v1alpha1.Group, error) {
	return crdstore.List[v1alpha1.Group](ctx, s.store, "", "")
}

// bffUserStore adapts crdstore.Backend + ResourceClient to local.userStore.
type bffUserStore struct {
	store  crdstore.Backend
	client controller.ResourceClient
}

func (s bffUserStore) GetUserCRD(ctx context.Context, name, ns string) (*v1alpha1.User, error) {
	return crdstore.Get[v1alpha1.User](ctx, s.store, name, ns)
}
func (s bffUserStore) ApplyUserCRD(ctx context.Context, u *v1alpha1.User) error {
	return crdstore.Apply[v1alpha1.User](ctx, s.store, u)
}
func (s bffUserStore) PatchUserStatus(ctx context.Context, name, ns string, st *v1alpha1.UserStatus) error {
	return crdstore.PatchStatus[v1alpha1.User](ctx, s.store, name, ns, st)
}
func (s bffUserStore) ClearUserPassword(ctx context.Context, name, ns string) error {
	return s.client.ClearUserPassword(ctx, name, ns)
}
func (s bffUserStore) ListGroupCRDs(ctx context.Context) ([]*v1alpha1.Group, error) {
	return crdstore.List[v1alpha1.Group](ctx, s.store, "", "")
}
