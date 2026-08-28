// Command operator is the Varroa controller reconciler. It drives the phase
// state machine (Pending→Provisioning→Running→Connected→Failed) over the NATS
// bus. It also serves the hibernation wake interstitial over HTTP; API, gRPC,
// and OIDC remain in the BFF and Gateway tiers. Controller work is distributed
// through active/active sharding.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/ca"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/controller/sharding"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/hibernation/wakeserver"
	"github.com/varroaci/varroa-jenkins/internal/logging"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
	"github.com/varroaci/varroa-jenkins/internal/preflight"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/telemetry"
	"github.com/varroaci/varroa-jenkins/internal/tenancy"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

var version = "dev"

func main() {
	busURL := flag.String("bus-url", "", "NATS bus URL (defaults to VARROA_BUS_URL env)")
	busUser := flag.String("bus-user", "operator", "NATS bus username")
	busPassFile := flag.String("bus-pass-file", "", "Path to NATS bus password file (overrides BUS_PASSWORD env)")
	busCAFile := flag.String("bus-ca-file", "", "Path to NATS bus CA certificate file")
	kubeconfigPath := flag.String("kubeconfig", "", "Kubeconfig path (bypasses in-cluster config)")
	oidcIssuer := flag.String("oidc-issuer", envDefault("VARROA_OIDC_ISSUER", ""), "OIDC issuer URL (for bundle variable injection)")
	oidcClientID := flag.String("oidc-client-id", envDefault("VARROA_OIDC_CLIENT_ID", "varroa"), "OIDC client ID")
	authMode := flag.String("auth-mode", envDefault("VARROA_AUTH_MODE", "oidc"), "Auth mode: oidc or local")
	bffURL := flag.String("bff-url", envDefault("VARROA_BFF_URL", ""), "BFF URL (required when auth-mode=local)")
	oidcClientSecret := flag.String("oidc-client-secret", envDefault("VARROA_OIDC_CLIENT_SECRET", ""), "OIDC client secret")
	oidcRedirectURL := flag.String("oidc-redirect-url", envDefault("VARROA_OIDC_REDIRECT_URL", ""), "OIDC redirect URL (for Varroa login link)")
	oidcUserClaim := flag.String("oidc-user-claim", envDefault("VARROA_OIDC_USER_CLAIM", "preferred_username,sub"), "comma-separated JWT claims for kind:User bindings")
	oidcGroupClaim := flag.String("oidc-group-claim", envDefault("VARROA_OIDC_GROUP_CLAIM", "groups"), "comma-separated JWT claims for kind:Group bindings")
	defaultRead := flag.Bool("default-read", false, "Allow any authenticated user to read controllers without explicit binding")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	logFormat := flag.String("log-format", "text", "Log format: text, json")
	maxConcurrent := flag.Int("max-concurrent-reconciles", envInt("VARROA_MAX_CONCURRENT_RECONCILES", 8), "Max concurrent Controller reconciles")
	shardCount := flag.String("shard-count", envDefault("VARROA_SHARD_COUNT", "256"), "Number of virtual shards for active/active controller sharding (must match across replicas)")
	wakePort := flag.Int("wake-port", envInt("VARROA_WAKE_PORT", 8082), "Hibernation wake HTTP server port")
	flag.Parse()

	var err error
	level, err := logging.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q: %v\n", *logLevel, err)
		os.Exit(1)
	}
	logger := logging.New(level, *logFormat, os.Stderr).With("binary", "operator")
	slog.SetDefault(logger)
	if *wakePort < 1 || *wakePort > 65535 {
		logger.Error("invalid wake server port", "port", *wakePort)
		os.Exit(1)
	}
	wakeEnabled := envBool("VARROA_WAKE_ENABLED", true)

	var cluster string
	cluster, err = bus.ClusterFromEnv()
	if err != nil {
		logger.Error("invalid cluster name", "error", err)
		os.Exit(1)
	}
	logger.Info("cluster identity", "cluster", cluster)

	if *authMode == "oidc" && strings.TrimSpace(*oidcClientSecret) == "" {
		logger.Error("VARROA_OIDC_CLIENT_SECRET must be set when auth mode is oidc (no default is provided)")
		os.Exit(1)
	}

	_ = os.Setenv("VARROA_SERVICE_NAME", "varroa-operator")
	telemetryShutdown := telemetry.InitTelemetry(context.Background())
	if !telemetry.Disabled() {
		logger = telemetry.LogHandler(logger)
		slog.SetDefault(logger)
	}
	defer func() {
		if telemetryShutdown != nil {
			if err := telemetryShutdown(context.Background()); err != nil {
				logger.Warn("telemetry shutdown failed", "error", err)
			}
		}
	}()

	if *busURL == "" {
		if u := os.Getenv("VARROA_BUS_URL"); u != "" {
			*busURL = u
		}
	}

	// Resolve bus credentials.
	busPassword := os.Getenv("BUS_PASSWORD")
	if *busPassFile != "" {
		passBytes, err := os.ReadFile(*busPassFile)
		if err != nil {
			logger.Error("failed to read bus password file", "path", *busPassFile, "error", err)
			os.Exit(1)
		}
		busPassword = strings.TrimSpace(string(passBytes))
	}

	operatorNamespace := controller.ResolveOperatorNamespace(logger)
	gatewayEndpoint := os.Getenv("VARROA_OPERATOR_ENDPOINT")
	if gatewayEndpoint == "" {
		gatewayEndpoint = "varroa-varroa-gateway.varroa-system.svc.cluster.local:9090"
	}

	// --- Resource client ---
	var clientsetClient *controller.ClientsetClient
	if *kubeconfigPath != "" {
		clientsetClient, err = controller.NewClientsetClientWithKubeconfig(*kubeconfigPath)
	} else {
		clientsetClient, err = controller.NewClientsetClient()
	}
	if err != nil {
		logger.Error("failed to create clientset client", "error", err)
		os.Exit(1)
	}

	// --- CA & token signer ---
	var certAuth *ca.CA
	caData, err := clientsetClient.GetSecret(context.Background(), "varroa-ca", operatorNamespace)
	if err == nil && len(caData["tls.crt"]) > 0 && len(caData["tls.key"]) > 0 {
		certAuth, err = ca.LoadCA(caData["tls.crt"], caData["tls.key"])
		if err != nil {
			logger.Error("failed to load CA from Secret", "error", err)
			os.Exit(1)
		}
		logger.Info("loaded existing CA from Secret", "namespace", operatorNamespace)
	} else {
		certAuth, err = ca.NewCA()
		if err != nil {
			logger.Error("failed to create CA", "error", err)
			os.Exit(1)
		}
		certPEM, keyPEM, err := certAuth.Persist()
		if err != nil {
			logger.Error("failed to persist CA", "error", err)
			os.Exit(1)
		}
		if err := clientsetClient.CreateOrUpdateSecret(context.Background(), "varroa-ca", operatorNamespace, map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		}); err != nil {
			logger.Warn("failed to persist CA to Secret", "error", err)
		} else {
			logger.Info("CA persisted to Secret", "namespace", operatorNamespace)
		}
	}
	tokenSigner := mitesrv.NewTokenSigner(certAuth.BootstrapHMACKey())

	// Create RS256 token signer for mite Jenkins auth JWT.
	// Persist the keypair in a Secret so it survives operator restarts.
	var miteTokenSigner *mitesrv.MiteTokenSigner
	keyData, err := clientsetClient.GetSecret(context.Background(), "varroa-mite-signing-key", operatorNamespace)
	if err == nil && len(keyData["private-key"]) > 0 {
		miteTokenSigner, err = mitesrv.NewMiteTokenSignerFromPEM(keyData["private-key"])
		if err != nil {
			logger.Error("failed to load mite signing key from Secret", "error", err)
			os.Exit(1)
		}
		logger.Info("loaded existing mite signing key from Secret",
			"namespace", operatorNamespace,
			"kid", miteTokenSigner.KID())
	} else {
		miteTokenSigner, err = mitesrv.NewMiteTokenSigner()
		if err != nil {
			logger.Error("failed to generate mite signing key", "error", err)
			os.Exit(1)
		}
		privPEM, err := miteTokenSigner.PrivateKeyPEM()
		if err != nil {
			logger.Error("failed to export mite signing key", "error", err)
			os.Exit(1)
		}
		if err := clientsetClient.CreateOrUpdateSecret(context.Background(), "varroa-mite-signing-key", operatorNamespace, map[string][]byte{
			"private-key": []byte(privPEM),
		}); err != nil {
			logger.Warn("failed to persist mite signing key to Secret", "error", err)
		} else {
			logger.Info("mite signing key persisted to Secret",
				"namespace", operatorNamespace,
				"kid", miteTokenSigner.KID())
		}
	}

	// --- NATS bus ---
	busConn, err := bus.Connect(*busURL, bus.Config{
		Username:    *busUser,
		Password:    busPassword,
		CAFile:      *busCAFile,
		InboxPrefix: "_INBOX_operator",
	})
	if err != nil {
		logger.Error("failed to connect to bus", "url", *busURL, "error", err)
		os.Exit(1)
	}
	defer busConn.Close()
	busConn.Logger = logger
	// JetStream replica count for streams and KV buckets (from the NATS
	// cluster size, clamped 1..3 by the chart). Streams/KV created below and by
	// downstream consumers replicate to this count so they survive a single
	// NATS pod loss. Values < 1 are clamped to 1 by SetReplicas.
	busConn.SetReplicas(envInt("VARROA_JETSTREAM_REPLICAS", 1))
	logger.Info("connected to NATS bus", "url", *busURL, "jetStreamReplicas", busConn.Replicas())

	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 5*time.Minute)
	if err != nil {
		logger.Error("failed to ensure KV bucket", "bucket", bus.KVSnapshotBucket, "error", err)
		os.Exit(1)
	}
	presenceKV, err := busConn.EnsureKV(bus.KVPresenceBucket, 90*time.Second)
	if err != nil {
		logger.Error("failed to ensure KV bucket", "bucket", bus.KVPresenceBucket, "error", err)
		os.Exit(1)
	}
	desiredKV, err := busConn.EnsureKV(bus.KVDesiredBucket, 0)
	if err != nil {
		logger.Error("failed to ensure KV bucket", "bucket", bus.KVDesiredBucket, "error", err)
		os.Exit(1)
	}
	clustersKV, err := busConn.EnsureKV(bus.KVClustersBucket, bus.ClusterEntryTTL)
	if err != nil {
		logger.Error("failed to ensure KV bucket", "bucket", bus.KVClustersBucket, "error", err)
		os.Exit(1)
	}

	miteTransport := transport.NewBusTransport(cluster, busConn, snapshotKV, presenceKV, desiredKV)
	miteTransport.Logger = logger

	// --- Bundle resolver ---
	resolver := bundle.NewResolver("/tmp/varroa-bundles")
	resolver.Logger = logger
	// In local auth mode the BFF is the OIDC issuer — Jenkins pods
	// discover /.well-known/jwks.json from the BFF to validate user tokens.
	if *authMode == "local" && *bffURL != "" {
		resolver.SetOIDCConfig(*bffURL, *oidcClientID, *oidcClientSecret)
		logger.Info("local auth bundle variables configured", "issuer", *bffURL)
		resolver.SetOIDCClaims(*oidcUserClaim, *oidcGroupClaim)
	} else if *oidcIssuer != "" {
		resolver.SetOIDCConfig(*oidcIssuer, *oidcClientID, *oidcClientSecret)
		logger.Info("OIDC bundle variables configured", "issuer", *oidcIssuer)
		resolver.SetOIDCClaims(*oidcUserClaim, *oidcGroupClaim)
	}

	// Create Composer for composed bundles (catalog-driven).
	composer := bundle.NewComposer(storeItemLookupAdapter{clientsetClient}, resolver, "/tmp/varroa-composed", *oidcIssuer, *oidcClientID, *oidcClientSecret, operatorNamespace)

	// Create catalog reconciler for CatalogSource sync and ComposedBundle
	// status updates. Uses the resolver's GitCloner for authenticated clones.
	var cloneCache *bundle.CloneCache
	if dir := os.Getenv("VARROA_GIT_CACHE_DIR"); dir != "" {
		maxRepos := envInt("VARROA_GIT_CACHE_MAX_REPOS", 50)
		maxSizeMiB := int64(envInt("VARROA_GIT_CACHE_MAX_SIZE_MIB", 2048))
		cc, err := bundle.NewCloneCache(dir, maxRepos, maxSizeMiB, logger)
		if err != nil {
			logger.Warn("git clone cache disabled", "dir", dir, "error", err)
		} else {
			cloneCache = cc
			resolver.UseCloneCache(cloneCache)
			logger.Info("git clone cache enabled", "dir", dir, "maxRepos", maxRepos, "maxSizeMiB", maxSizeMiB)
		}
	}
	// The catalog reconciler needs the update-center base URL for the reserved
	// varroa-update-center source, and its OWN timed HTTP client: the catalog
	// ticker is leader-only and reconciles every source serially, so a hung
	// fetch through an untimed client would stop all catalog syncing.
	catalogReconciler := controller.NewCatalogReconciler(
		clientsetClient, clientsetClient, resolver.Cloner(), resolver, cloneCache,
		"/tmp/varroa-catalogs", logger, operatorNamespace,
		os.Getenv("VARROA_UPDATE_CENTER_URL"), &http.Client{Timeout: 30 * time.Second})

	// Create JenkinsVersionProfile reconciler for plugin set materialization.
	versionProfileReconciler := controller.NewJenkinsVersionProfileReconciler(clientsetClient, clientsetClient, operatorNamespace, logger)

	// Create Team reconciler for composing Teams into Group + VarroaRoleBinding.
	teamReconciler := controller.NewTeamReconciler(clientsetClient, clientsetClient, operatorNamespace, logger)

	// --- RBAC informers ---
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
		&unstructured.Unstructured{}, 0, cache.Indexers{},
	)
	if err := roleInformer.SetTransform(func(obj interface{}) (interface{}, error) {
		u, _ := obj.(*unstructured.Unstructured)
		if u == nil {
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
		&unstructured.Unstructured{}, 0,
		cache.Indexers{rbac.BySubjectIndex: rbac.SubjectIndexFunc},
	)
	if err := roleBindingInformer.SetTransform(func(obj interface{}) (interface{}, error) {
		u, _ := obj.(*unstructured.Unstructured)
		if u == nil {
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
		&unstructured.Unstructured{}, 0, cache.Indexers{},
	)
	if err := controllerInformer.SetTransform(func(obj interface{}) (interface{}, error) {
		u, _ := obj.(*unstructured.Unstructured)
		if u == nil {
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

	userClaims := strings.Split(*oidcUserClaim, ",")
	groupClaims := strings.Split(*oidcGroupClaim, ",")
	rbacResolver := rbac.NewResolver(roleInformer, roleBindingInformer, jenkinsRoleInformer, jenkinsRoleBindingInformer, controllerInformer,
		*defaultRead, userClaims, groupClaims)
	rbacGen := rbac.NewGenerator(rbacResolver)
	rbacGen.Logger = logger

	// --- Reconciler ---
	reconciler := controller.NewReconciler(resolver, clientsetClient, clientsetClient, miteTransport, tokenSigner, rbacGen, composer)
	reconciler.SetClusterName(cluster)
	reconciler.SetMiteTokenSigner(miteTokenSigner)
	reconciler.SetCAPEM(string(certAuth.CAPEM()))
	// Wire the bus-mode token-grant path: a mite's TokenRefreshRequest
	// (forwarded over the bus) is answered with a freshly minted TokenGrant.
	// Without this the BusTransport's handleTokenRefresh is a no-op and mites
	// never obtain a Jenkins token, falling through to anonymous (HTTP 403).
	if miteTokenSigner != nil {
		miteTransport.TokenGrantFunc = func(ns, name string) (string, int64, error) {
			return reconciler.MintMiteTokenForce(ns + "/" + name)
		}
		logger.Info("operator bus token-grant path wired (force-mint)")
	} else {
		logger.Warn("operator bus token-grant path NOT wired (no mite token signer)")
	}
	reconciler.SetVarroaEndpoint(gatewayEndpoint)
	reconciler.SetOperatorNamespace(operatorNamespace)
	reconciler.SetWakeServerPort(int32(*wakePort), wakeEnabled)
	managedNamespaces := os.Getenv("MANAGED_NAMESPACES")
	set := tenancy.NewManagedSet(managedNamespaces, operatorNamespace)
	reconciler.SetTenancy(tenancy.NewClassifier(clientsetClient, set))
	teamReconciler.SetManagedSet(set)
	reconciler.SetVarroaRedirectURL(*oidcRedirectURL)
	reconciler.Logger = logger

	// --- Activity publisher (mode-agnostic, publishes to activity.* subjects) ---
	actPub := activity.NewPublisher(cluster, busConn)
	reconciler.SetActivityPublisher(actPub)

	// --- Controller-runtime manager ---
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	// Route controller-runtime's internal logging (reconciler errors,
	// reflector failures) through our slog logger — without this every
	// reconcile error is silently discarded.
	crlog.SetLogger(logr.FromSlogHandler(logger.Handler()))

	// In managedNamespaces mode the operator holds only per-namespace RBAC, so
	// the informers for the namespaced resources it Owns (BroodSchedule owns
	// CronJob + Secret) must be scoped to those namespaces — a cluster-wide
	// list/watch would be forbidden. Cluster-scoped and Controller watches are
	// left untouched.
	cacheOpts := crcache.Options{}
	if nsList := set.Namespaces(); len(nsList) > 0 {
		byNs := make(map[string]crcache.Config, len(nsList))
		for _, ns := range nsList {
			byNs[ns] = crcache.Config{}
		}
		cacheOpts.ByObject = map[crclient.Object]crcache.ByObject{
			&batchv1.CronJob{}: {Namespaces: byNs},
			&corev1.Secret{}:   {Namespaces: byNs},
		}
		logger.Info("scoped CronJob/Secret informer cache to managed namespaces", "namespaces", nsList)
	}

	mgr, err := manager.New(clientsetClient.RESTConfig(), manager.Options{
		Scheme:                  scheme,
		Cache:                   cacheOpts,
		HealthProbeBindAddress:  ":8090",
		LeaderElection:          true,
		LeaderElectionID:        "varroa-operator.varroa.dev",
		LeaderElectionNamespace: operatorNamespace,
		// Use controller-runtime defaults: LeaseDuration 15s, RenewDeadline 10s, RetryPeriod 2s.
	})
	if err != nil {
		logger.Error("failed to create manager", "error", err)
		os.Exit(1)
	}

	reconciler.SetMaxConcurrentReconciles(*maxConcurrent)

	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error("failed to set up reconciler with manager", "error", err)
		os.Exit(1)
	}

	// --- BroodOperation reconciler ---
	broodRec := controller.NewBroodOperationReconciler(
		mgr.GetClient(),
		scheme,
		operatorNamespace,
		clientsetClient,
		clientsetClient,
		func(namespace, name string) { reconciler.WakeController(cluster, namespace, name) },
		func(namespace, name string) { reconciler.Reprovision(cluster, namespace, name) },
		actPub,
		logger,
	)
	if err := broodRec.SetupWithManager(mgr); err != nil {
		logger.Error("failed to set up brood operation reconciler", "error", err)
		os.Exit(1)
	}
	broodRec.SetOperatorTokenSigner(miteTokenSigner)

	// --- BroodSchedule reconciler ---
	schedRec := controller.NewBroodScheduleReconciler(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		scheme,
		operatorNamespace,
		miteTokenSigner,
		logger,
	)
	if err := schedRec.SetupWithManager(mgr); err != nil {
		logger.Error("failed to set up brood schedule reconciler", "error", err)
		os.Exit(1)
	}

	// --- Shard manager (active/active controller sharding) ---
	n, err := strconv.Atoi(*shardCount)
	if err != nil || n < 1 {
		logger.Warn("invalid shard-count, using default", "value", *shardCount, "default", sharding.DefaultShards)
		n = sharding.DefaultShards
	}
	ring := sharding.NewRing(n)
	shardSet := sharding.NewShardSet()
	reconciler.SetShardOwnership(ring, shardSet)

	identity, hostErr := os.Hostname()
	if hostErr != nil {
		identity = fmt.Sprintf("operator-%d", os.Getpid())
		logger.Warn("failed to get hostname for shard manager identity", "error", hostErr, "fallback", identity)
	}
	sm := sharding.NewShardManager(
		clientsetClient.Clientset().CoordinationV1().Leases(operatorNamespace),
		operatorNamespace,
		identity,
		ring,
		shardSet,
		reconciler.EnqueueShards,
		logger,
	)
	if err := mgr.Add(sm); err != nil {
		logger.Error("failed to add shard manager", "error", err)
		os.Exit(1)
	}
	logger.Info("shard manager registered", "shardCount", n, "identity", identity)

	// --- Lifecycle store for drain state ---
	lifecycleStore := controller.NewLifecycleStore(clientsetClient.Clientset(), operatorNamespace, logger.With("component", "lifecycle"))
	if _, err := lifecycleStore.Load(context.Background()); err != nil {
		logger.Warn("lifecycle store initial load failed", "error", err)
	}

	// Register BFF command subscribers as a leader-elected runnable so only
	// the active replica processes reconcile/wake/approveRestart requests.
	crud := &controller.CommandCRUD{
		Client:            clientsetClient,
		Store:             clientsetClient,
		OperatorNamespace: operatorNamespace,
		ManagedNamespaces: managedNamespaces,
		Lifecycle:         lifecycleStore,
		Logger:            logger.With("component", "command_crud"),
	}
	crud.PreflightCheck = func(ctx context.Context, deps controller.PreflightDepsInterface, draft *v1alpha1.Controller, inlineBundle *v1alpha1.ComposedBundleSpec, opts controller.PreflightOptions) []bus.Check {
		pres := preflight.Run(ctx, deps, draft, inlineBundle, preflight.Options{
			OperatorNamespace: opts.OperatorNamespace,
			ManagedNamespaces: opts.ManagedNamespaces,
			ForUpdate:         opts.ForUpdate,
			PriorVersion:      opts.PriorVersion,
		})
		result := make([]bus.Check, len(pres))
		for i, c := range pres {
			result[i] = bus.Check{ID: c.ID, Status: c.Status, Message: c.Message}
		}
		return result
	}
	// Pre-declare hbRunner so the operatorCommandRunner can reference its BeatNow method.
	// The struct value is assigned after the command runner block, so beatNow must be a
	// closure over the hbRunner variable — a bare `hbRunner.BeatNow` method value would
	// bind the nil receiver captured here and panic on every call.
	var hbRunner *clusterHeartbeatRunner
	broodOps := controller.NewCommandBroodOps(mgr.GetClient(), operatorNamespace, logger)
	broodSchedules := controller.NewCommandBroodSchedules(mgr.GetClient(), operatorNamespace, logger)
	configCrud := controller.NewConfigCRUD(clientsetClient, clientsetClient, composer, operatorNamespace, logger.With("component", "config_crud"))
	if err := mgr.Add(&operatorCommandRunner{
		busConn:        busConn,
		cluster:        cluster,
		reconciler:     reconciler,
		crud:           crud,
		configCrud:     configCrud,
		broodOps:       broodOps,
		broodSchedules: broodSchedules,
		lifecycle:      lifecycleStore,
		activity:       actPub,
		beatNow:        func() { hbRunner.BeatNow() },
		logger:         logger,
	}); err != nil {
		logger.Error("failed to add operator command runner", "error", err)
		os.Exit(1)
	}

	// Register cluster heartbeat runner (leader-elected, 30s tick).
	hbRunner = &clusterHeartbeatRunner{
		clusterName: cluster,
		version:     version,
		clustersKV:  clustersKV,
		client:      clientsetClient,
		store:       clientsetClient,
		lifecycle:   lifecycleStore,
		beatNow:     make(chan struct{}, 1),
		logger:      logger,
	}
	if err := mgr.Add(hbRunner); err != nil {
		logger.Error("failed to add cluster heartbeat runner", "error", err)
		os.Exit(1)
	}

	// Register cluster drain runner (leader-elected, 15s tick).
	if err := mgr.Add(&clusterDrainRunner{
		clusterName: cluster,
		lifecycle:   lifecycleStore,
		client:      clientsetClient,
		store:       clientsetClient,
		activity:    actPub,
		beatNow:     hbRunner.BeatNow,
		logger:      logger.With("component", "drain_runner"),
	}); err != nil {
		logger.Error("failed to add cluster drain runner", "error", err)
		os.Exit(1)
	}

	// Register health/readiness checks for k8s probes.
	if err := mgr.AddHealthzCheck("healthz", func(_ *http.Request) error { return nil }); err != nil {
		logger.Error("failed to add healthz check", "error", err)
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", func(_ *http.Request) error { return nil }); err != nil {
		logger.Error("failed to add readyz check", "error", err)
		os.Exit(1)
	}

	// --- Start ---
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start RBAC informers.
	go roleInformer.Run(ctx.Done())
	go roleBindingInformer.Run(ctx.Done())
	go jenkinsRoleInformer.Run(ctx.Done())
	go jenkinsRoleBindingInformer.Run(ctx.Done())
	go controllerInformer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(),
		roleInformer.HasSynced,
		jenkinsRoleInformer.HasSynced,
		jenkinsRoleBindingInformer.HasSynced,
		roleBindingInformer.HasSynced,
		controllerInformer.HasSynced,
	) {
		logger.Error("failed to sync RBAC informer caches")
		os.Exit(1)
	}
	logger.Info("RBAC informers synced")

	// --- Auxiliary reconciler runnables (leader-elected) ---
	// These replace the pre-leader-gating raw goroutines: each ticks only
	// while this replica holds the leader lease, instead of running
	// unconditionally in every replica.

	// 1. Catalog reconciler (catalog sources + composed bundles, 15s).
	if err := mgr.Add(&tickerRunnable{
		name:     "catalog",
		interval: 15 * time.Second,
		tick: func(ctx context.Context) {
			sources, err := crdstore.List[v1alpha1.CatalogSource](ctx, clientsetClient, "", "")
			if err != nil {
				logger.Warn("failed to list catalog sources", "error", err)
			} else {
				for _, src := range sources {
					catalogReconciler.Reconcile(ctx, src)
				}
			}
			bundles, err := crdstore.List[v1alpha1.ComposedBundle](ctx, clientsetClient, "", "")
			if err != nil {
				logger.Warn("failed to list composed bundles", "error", err)
			} else {
				for _, cb := range bundles {
					catalogReconciler.ReconcileComposedBundle(ctx, cb)
				}
			}
		},
		logger: logger,
	}); err != nil {
		logger.Error("failed to add catalog runnable", "error", err)
		os.Exit(1)
	}

	// 1b. Starter bundle seeder (60s, immediate). Runs ahead of the catalog tick
	// on interval only by luck, which is fine: the catalog ticker composes
	// whatever ComposedBundles exist when it fires, so a bundle seeded a moment
	// later is simply composed on the following pass.
	starterReconciler := controller.NewStarterReconciler(clientsetClient, operatorNamespace, logger)
	if err := mgr.Add(&tickerRunnable{
		name:      "starter-bundle",
		interval:  60 * time.Second,
		immediate: true,
		tick: func(ctx context.Context) {
			if err := starterReconciler.Reconcile(ctx); err != nil {
				logger.Warn("starter bundle seeder tick", "error", err)
			}
		},
		logger: logger,
	}); err != nil {
		logger.Error("failed to add starter bundle runnable", "error", err)
		os.Exit(1)
	}

	// 2. JenkinsVersionProfile reconciler (30s, immediate).
	if err := mgr.Add(&tickerRunnable{
		name:      "version-profile",
		interval:  30 * time.Second,
		immediate: true,
		tick: func(ctx context.Context) {
			reconcileAllVersionProfiles(ctx, clientsetClient, versionProfileReconciler, logger)
		},
		logger: logger,
	}); err != nil {
		logger.Error("failed to add version-profile runnable", "error", err)
		os.Exit(1)
	}

	// 3. Team reconciler (30s, immediate).
	if err := mgr.Add(&tickerRunnable{
		name:      "team",
		interval:  30 * time.Second,
		immediate: true,
		tick: func(ctx context.Context) {
			reconcileAllTeams(ctx, clientsetClient, teamReconciler, logger)
		},
		logger: logger,
	}); err != nil {
		logger.Error("failed to add team runnable", "error", err)
		os.Exit(1)
	}

	// 4. Built-in role reconciler (60s, immediate).
	roleRec := controller.NewRoleReconciler(clientsetClient, clientsetClient)
	roleRec.Logger = logger
	if err := mgr.Add(&tickerRunnable{
		name:      "role",
		interval:  60 * time.Second,
		immediate: true,
		tick: func(ctx context.Context) {
			if err := roleRec.Reconcile(ctx); err != nil {
				logger.Warn("role reconciler tick", "error", err)
			}
		},
		logger: logger,
	}); err != nil {
		logger.Error("failed to add role runnable", "error", err)
		os.Exit(1)
	}

	// 5–6. Local-only auth reconcilers and bootstrap.
	if *authMode == "local" {
		userRec := controller.NewUserReconciler(clientsetClient, clientsetClient, operatorNamespace)
		userRec.Logger = logger
		if err := mgr.Add(&tickerRunnable{
			name:     "user",
			interval: 15 * time.Second,
			setup: func(ctx context.Context) error {
				return controller.BootstrapLocalAdmin(ctx, clientsetClient, clientsetClient, operatorNamespace, logger)
			},
			tick: func(ctx context.Context) {
				if err := userRec.Reconcile(ctx); err != nil {
					logger.Warn("user reconciler tick", "error", err)
				}
			},
			logger: logger,
		}); err != nil {
			logger.Error("failed to add user runnable", "error", err)
			os.Exit(1)
		}

		groupRec := controller.NewGroupReconciler(clientsetClient, clientsetClient, operatorNamespace)
		groupRec.Logger = logger
		if err := mgr.Add(&tickerRunnable{
			name:     "group",
			interval: 60 * time.Second,
			tick: func(ctx context.Context) {
				if err := groupRec.Reconcile(ctx); err != nil {
					logger.Warn("group reconciler tick", "error", err)
				}
			},
			logger: logger,
		}); err != nil {
			logger.Error("failed to add group runnable", "error", err)
			os.Exit(1)
		}
	}

	// 7. UpdateCenter reconciler (30s, immediate, leader-elected) — wired only
	// when the in-cluster update center is enabled (VARROA_UPDATE_CENTER_URL is
	// injected by the chart operator deployment when updateCenter.enabled). It
	// reconciles the singleton UpdateCenter CR (storage readiness, seed import,
	// coverage gaps, phase) and points the Controller reconciler at the UC so it
	// rewires each controller's plugins-init JENKINS_UC* env.
	if ucURL := os.Getenv("VARROA_UPDATE_CENTER_URL"); ucURL != "" {
		ucRec := controller.NewUpdateCenterReconciler(clientsetClient, clientsetClient, operatorNamespace, ucURL, logger)
		if err := ucRec.SetupWithManager(mgr); err != nil {
			logger.Error("failed to set up update-center reconciler", "error", err)
			os.Exit(1)
		}
		reconciler.SetUpdateCenterBaseURL(ucURL)
		logger.Info("update-center integration enabled", "url", ucURL)
	}

	// 8. ProvisioningDefaults refresh (30s, immediate). Runs on EVERY replica:
	// the sharded Controller reconciler reconciles on non-leaders too, and
	// each replica needs its own defaults cache (read-only fetch, no writes).
	if err := mgr.Add(&tickerRunnable{
		name:         "provisioning-defaults",
		interval:     30 * time.Second,
		immediate:    true,
		everyReplica: true,
		tick: func(ctx context.Context) {
			d, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, clientsetClient, "varroa-defaults", "")
			if err != nil {
				logger.Warn("provisioning defaults refresh", "error", err)
				return
			}
			reconciler.SetProvisioningDefaults(d)
		},
		logger: logger,
	}); err != nil {
		logger.Error("failed to add provisioning-defaults runnable", "error", err)
		os.Exit(1)
	}

	// 9. Shard resync safety net (10m, every replica): re-enqueues owned
	// controllers so a shard acquisition that raced a failed list or full
	// event channel cannot strand its controllers indefinitely.
	if err := mgr.Add(&tickerRunnable{
		name:     "shard-resync",
		interval: 10 * time.Minute,
		// De-phase from the 10s shard-rebalance loop and from other replicas
		// so resync relists and acquisition enqueues don't storm together.
		jitter:       time.Minute,
		everyReplica: true,
		tick: func(_ context.Context) {
			reconciler.ResyncOwned()
		},
		logger: logger,
	}); err != nil {
		logger.Error("failed to add shard-resync runnable", "error", err)
		os.Exit(1)
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", telemetry.MetricsAuthMiddleware(promhttp.Handler()))
	metricsMux.Handle("/healthz", telemetry.HealthzHandler())
	metricsSrv := &http.Server{Addr: ":9091", Handler: metricsMux}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server error", "error", err)
		}
	}()
	defer func() {
		if err := metricsSrv.Shutdown(context.Background()); err != nil {
			logger.Warn("metrics server shutdown error", "error", err)
		}
	}()

	if wakeEnabled {
		primeWakeRootDomain(ctx, clientsetClient, reconciler, logger)
		wakeSrv := &http.Server{
			Addr: ":" + strconv.Itoa(*wakePort),
			Handler: &wakeserver.Server{
				Lister:     controllerStoreLister{store: controllerInformer.GetStore()},
				Waker:      reconciler,
				RootDomain: reconciler.RootDomain,
				Logger:     logger.With("component", "wake_server"),
			},
		}
		go func() {
			if err := wakeSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("wake server error", "error", err)
			}
		}()
		defer func() {
			if err := wakeSrv.Shutdown(context.Background()); err != nil {
				logger.Warn("wake server shutdown error", "error", err)
			}
		}()
	}

	logger.Info("operator starting", "namespace", operatorNamespace, "bus", *busURL)
	if err := mgr.Start(ctx); err != nil {
		logger.Error("manager exited with error", "error", err)
		os.Exit(1)
	}
	logger.Info("operator shut down")
}

// operatorCommandRunner subscribes to NATS request-reply subjects for BFF
// commands (reconcile, nudge, wake, hibernate, approveRestart) and controller
// CRUD operations.
// It implements manager.LeaderElectionRunnable so only the elected leader
// processes requests; non-leaders leave the queue group, letting NATS route
// commands exclusively to the active replica.
type operatorCommandRunner struct {
	busConn        *bus.Conn
	cluster        string
	reconciler     *controller.Reconciler
	crud           *controller.CommandCRUD
	configCrud     *controller.ConfigCRUD
	broodOps       *controller.CommandBroodOps
	broodSchedules *controller.CommandBroodSchedules
	lifecycle      *controller.LifecycleStore
	activity       *activity.Publisher
	beatNow        func()
	logger         *slog.Logger
}

func (r *operatorCommandRunner) NeedLeaderElection() bool { return true }

// hibernationCommandRunner is the narrow slice of *controller.Reconciler the
// authenticated hibernate and wake bus handlers need, kept as an interface so
// the error-code mapping is unit-testable without a full Reconciler.
type hibernationCommandRunner interface {
	HibernateController(ctx context.Context, namespace, name string) error
	WakeControllerAction(ctx context.Context, namespace, name string) error
}

// hibernationCommandErrorCode maps an authenticated hibernate/wake action
// error to a bus error code. ErrControllerStopped is a conflict (hard power
// beats hibernation); a missing Controller is 404, matching the route contract
// in api/openapi/paths/controllers.yaml; everything else is internal.
func hibernationCommandErrorCode(err error) string {
	switch {
	case errors.Is(err, controller.ErrControllerStopped):
		return bus.CodeConflict
	case apierrors.IsNotFound(err):
		return bus.CodeNotFound
	default:
		return bus.CodeInternal
	}
}

// handleHibernate implements the operator.<cluster>.hibernate request-reply
// handler.
func handleHibernate(ctx context.Context, logger *slog.Logger, rec hibernationCommandRunner, data []byte) []byte {
	var req bus.HibernateRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("hibernate request decode failed", "error", err)
		resp, _ := json.Marshal(bus.HibernateResponse{Error: "invalid request", Code: bus.CodeInvalid})
		return resp
	}
	if err := rec.HibernateController(ctx, req.Namespace, req.Name); err != nil {
		logger.Warn("hibernate failed",
			"controller", req.Namespace+"/"+req.Name,
			"error", err)
		resp, _ := json.Marshal(bus.HibernateResponse{Error: err.Error(), Code: hibernationCommandErrorCode(err)})
		return resp
	}
	resp, _ := json.Marshal(bus.HibernateResponse{})
	return resp
}

// handleWake implements the operator.<cluster>.wake request-reply handler.
func handleWake(ctx context.Context, logger *slog.Logger, rec hibernationCommandRunner, data []byte) []byte {
	var req bus.WakeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("wake request decode failed", "error", err)
		resp, _ := json.Marshal(bus.WakeResponse{Error: "invalid request", Code: bus.CodeInvalid})
		return resp
	}
	if err := rec.WakeControllerAction(ctx, req.Namespace, req.Name); err != nil {
		logger.Warn("wake failed",
			"controller", req.Namespace+"/"+req.Name,
			"error", err)
		resp, _ := json.Marshal(bus.WakeResponse{Error: err.Error(), Code: hibernationCommandErrorCode(err)})
		return resp
	}
	resp, _ := json.Marshal(bus.WakeResponse{})
	return resp
}

func (r *operatorCommandRunner) Start(ctx context.Context) error {
	const operatorQueue = "operator-workers"

	// reconcileAction wraps a fire-and-forget reconciler call in the shared
	// ReconcileRequest decode/response envelope.
	reconcileAction := func(name string, act func(req bus.ReconcileRequest)) func([]byte) []byte {
		return func(data []byte) []byte {
			var req bus.ReconcileRequest
			if err := json.Unmarshal(data, &req); err != nil {
				r.logger.Warn(name+" request decode failed", "error", err)
				resp, _ := json.Marshal(bus.ReconcileResponse{Error: err.Error()})
				return resp
			}
			act(req)
			resp, _ := json.Marshal(bus.ReconcileResponse{})
			return resp
		}
	}

	subscriptions := []struct {
		name    string
		subject string
		handler bus.RequestHandler
	}{
		{"reconcile", bus.OperatorReconcileSubject(r.cluster), reconcileAction("reconcile", func(req bus.ReconcileRequest) {
			r.reconciler.TriggerReconcile(r.cluster, req.Name, req.Namespace)
		})},
		{"nudge", bus.OperatorNudgeSubject(r.cluster), reconcileAction("nudge", func(req bus.ReconcileRequest) {
			r.reconciler.WakeController(r.cluster, req.Namespace, req.Name)
		})},
		{"hibernate", bus.OperatorHibernateSubject(r.cluster), func(data []byte) []byte {
			return handleHibernate(ctx, r.logger, r.reconciler, data)
		}},
		{"wake", bus.OperatorWakeSubject(r.cluster), func(data []byte) []byte {
			return handleWake(ctx, r.logger, r.reconciler, data)
		}},
		{"reprovision", bus.OperatorReprovisionSubject(r.cluster), reconcileAction("reprovision", func(req bus.ReconcileRequest) {
			r.reconciler.Reprovision(r.cluster, req.Namespace, req.Name)
		})},
		{"approve restart", bus.OperatorApproveSubject(r.cluster), func(data []byte) []byte {
			var req bus.ApproveRestartRequest
			if err := json.Unmarshal(data, &req); err != nil {
				r.logger.Warn("approve restart request decode failed", "error", err)
				resp, _ := json.Marshal(bus.ApproveRestartResponse{Error: "invalid request"})
				return resp
			}
			if err := r.reconciler.ApproveRestart(context.Background(), r.cluster, req.Namespace, req.Name, req.Action); err != nil {
				r.logger.Warn("approve restart failed",
					"controller", req.Namespace+"/"+req.Name,
					"action", req.Action, "error", err)
				resp, _ := json.Marshal(bus.ApproveRestartResponse{Error: err.Error()})
				return resp
			}
			resp, _ := json.Marshal(bus.ApproveRestartResponse{})
			return resp
		}},
		{"approve deletion", bus.OperatorApproveDeletionSubject(r.cluster), func(data []byte) []byte {
			var req bus.ApproveDeletionRequest
			if err := json.Unmarshal(data, &req); err != nil {
				r.logger.Warn("approve deletion request decode failed", "error", err)
				resp, _ := json.Marshal(bus.ApproveDeletionResponse{Error: "invalid request"})
				return resp
			}
			if err := r.reconciler.ApproveDeletion(context.Background(), r.cluster, req.Namespace, req.Name, req.Path); err != nil {
				r.logger.Warn("approve deletion failed",
					"controller", req.Namespace+"/"+req.Name,
					"path", req.Path, "error", err)
				resp, _ := json.Marshal(bus.ApproveDeletionResponse{Error: err.Error()})
				return resp
			}
			resp, _ := json.Marshal(bus.ApproveDeletionResponse{})
			return resp
		}},
		{"controllers.list", bus.OperatorControllersSubject(r.cluster, "list"), r.crud.HandleList},
		{"controllers.get", bus.OperatorControllersSubject(r.cluster, "get"), r.crud.HandleGet},
		{"controllers.create", bus.OperatorControllersSubject(r.cluster, "create"), r.crud.HandleCreate},
		{"controllers.update", bus.OperatorControllersSubject(r.cluster, "update"), r.crud.HandleUpdate},
		{"controllers.delete", bus.OperatorControllersSubject(r.cluster, "delete"), r.crud.HandleDelete},
		{"controllers.deletepod", bus.OperatorControllersSubject(r.cluster, "deletepod"), r.crud.HandleDeletePod},
		{"cluster.drain", bus.OperatorClusterSubject(r.cluster, "drain"), func(data []byte) []byte {
			var req bus.ClusterDrainRequest
			if err := json.Unmarshal(data, &req); err != nil {
				resp, _ := json.Marshal(bus.ClusterDrainResponse{Error: "invalid request", Code: bus.CodeInvalid})
				return resp
			}
			bctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			st, err := r.lifecycle.Load(bctx)
			if err != nil {
				r.logger.Warn("cluster.drain: load failed", "error", err)
				resp, _ := json.Marshal(bus.ClusterDrainResponse{Error: err.Error(), Code: bus.CodeInternal})
				return resp
			}

			if st.State == bus.ClusterStateActive {
				if err := r.lifecycle.SetDraining(bctx, req.RequestedBy); err != nil {
					r.logger.Warn("cluster.drain: set draining failed", "error", err)
					resp, _ := json.Marshal(bus.ClusterDrainResponse{Error: err.Error(), Code: bus.CodeInternal})
					return resp
				}

				// Count controllers for activity event
				ctrlCount := 0
				if ctrls, listErr := crdstore.List[v1alpha1.Controller](bctx, r.crud.Store, "", ""); listErr == nil {
					ctrlCount = len(ctrls)
				}

				if r.activity != nil {
					r.activity.Publish(activity.Event{
						Type:    "cluster.drain.started",
						Source:  "operator",
						Actor:   req.RequestedBy,
						Message: fmt.Sprintf("cluster drain started (%d controllers)", ctrlCount),
					})
				}
				if r.beatNow != nil {
					r.beatNow()
				}
				resp, _ := json.Marshal(bus.ClusterDrainResponse{State: bus.ClusterStateDraining})
				return resp
			}

			// Idempotent: already draining/drained, reply current state with no side effects.
			resp, _ := json.Marshal(bus.ClusterDrainResponse{State: st.State})
			return resp
		}},
		{"cluster.draincancel", bus.OperatorClusterSubject(r.cluster, "draincancel"), func(data []byte) []byte {
			var req bus.ClusterDrainCancelRequest
			if err := json.Unmarshal(data, &req); err != nil {
				resp, _ := json.Marshal(bus.ClusterDrainCancelResponse{Error: "invalid request", Code: bus.CodeInvalid})
				return resp
			}
			bctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			st, err := r.lifecycle.Load(bctx)
			if err != nil {
				r.logger.Warn("cluster.draincancel: load failed", "error", err)
				resp, _ := json.Marshal(bus.ClusterDrainCancelResponse{Error: err.Error(), Code: bus.CodeInternal})
				return resp
			}

			if st.State == bus.ClusterStateActive {
				resp, _ := json.Marshal(bus.ClusterDrainCancelResponse{
					Error: "cluster is not draining",
					Code:  bus.CodeConflict,
				})
				return resp
			}

			// draining or drained → set active
			if err := r.lifecycle.SetActive(bctx); err != nil {
				r.logger.Warn("cluster.draincancel: set active failed", "error", err)
				resp, _ := json.Marshal(bus.ClusterDrainCancelResponse{Error: err.Error(), Code: bus.CodeInternal})
				return resp
			}

			if r.activity != nil {
				r.activity.Publish(activity.Event{
					Type:    "cluster.drain.canceled",
					Source:  "operator",
					Actor:   req.RequestedBy,
					Message: "cluster drain canceled",
				})
			}
			if r.beatNow != nil {
				r.beatNow()
			}
			resp, _ := json.Marshal(bus.ClusterDrainCancelResponse{State: bus.ClusterStateActive})
			return resp
		}},
		// BroodOps command handlers per design §3 (F2 cross-cluster brood ops).
		{"broodops.create", bus.OperatorBroodOpsSubject(r.cluster, "create"), r.broodOps.HandleCreate},
		{"broodops.get", bus.OperatorBroodOpsSubject(r.cluster, "get"), r.broodOps.HandleGet},
		{"broodops.list", bus.OperatorBroodOpsSubject(r.cluster, "list"), r.broodOps.HandleList},
		{"broodops.cancel", bus.OperatorBroodOpsSubject(r.cluster, "cancel"), r.broodOps.HandleCancel},
		{"broodops.suspend", bus.OperatorBroodOpsSubject(r.cluster, "suspend"), r.broodOps.HandleSuspend},
		{"broodops.preview", bus.OperatorBroodOpsSubject(r.cluster, "preview"), r.broodOps.HandlePreview},
		// BroodSchedules command handlers.
		{"broodschedules.create", bus.OperatorBroodSchedulesSubject(r.cluster, "create"), r.broodSchedules.HandleCreate},
		{"broodschedules.get", bus.OperatorBroodSchedulesSubject(r.cluster, "get"), r.broodSchedules.HandleGet},
		{"broodschedules.list", bus.OperatorBroodSchedulesSubject(r.cluster, "list"), r.broodSchedules.HandleList},
		{"broodschedules.delete", bus.OperatorBroodSchedulesSubject(r.cluster, "delete"), r.broodSchedules.HandleDelete},
		{"broodschedules.suspend", bus.OperatorBroodSchedulesSubject(r.cluster, "suspend"), r.broodSchedules.HandleSuspend},
		// Config CRUD handlers (add-remote-config-authoring) — 27 subjects.
		{"bundles.list", bus.OperatorBundlesSubject(r.cluster, "list"), r.configCrud.HandleBundlesList},
		{"bundles.get", bus.OperatorBundlesSubject(r.cluster, "get"), r.configCrud.HandleBundlesGet},
		{"bundles.create", bus.OperatorBundlesSubject(r.cluster, "create"), r.configCrud.HandleBundlesCreate},
		{"bundles.update", bus.OperatorBundlesSubject(r.cluster, "update"), r.configCrud.HandleBundlesUpdate},
		{"bundles.delete", bus.OperatorBundlesSubject(r.cluster, "delete"), r.configCrud.HandleBundlesDelete},
		{"bundles.preview", bus.OperatorBundlesSubject(r.cluster, "preview"), r.configCrud.HandleBundlesPreview},
		{"bundles.validate", bus.OperatorBundlesSubject(r.cluster, "validate"), r.configCrud.HandleBundlesValidate},
		{"bundles.pause", bus.OperatorBundlesSubject(r.cluster, "pause"), r.configCrud.HandleBundlesPause},
		{"bundles.resume", bus.OperatorBundlesSubject(r.cluster, "resume"), r.configCrud.HandleBundlesPause},
		{"catalog.itemlist", bus.OperatorCatalogSubject(r.cluster, "itemlist"), r.configCrud.HandleItemsList},
		{"catalog.itemget", bus.OperatorCatalogSubject(r.cluster, "itemget"), r.configCrud.HandleItemsGet},
		{"catalog.sourcelist", bus.OperatorCatalogSubject(r.cluster, "sourcelist"), r.configCrud.HandleSourcesList},
		{"catalog.sourceget", bus.OperatorCatalogSubject(r.cluster, "sourceget"), r.configCrud.HandleSourcesGet},
		{"catalog.sourcecreate", bus.OperatorCatalogSubject(r.cluster, "sourcecreate"), r.configCrud.HandleSourcesCreate},
		{"catalog.sourceupdate", bus.OperatorCatalogSubject(r.cluster, "sourceupdate"), r.configCrud.HandleSourcesUpdate},
		{"catalog.sourcedelete", bus.OperatorCatalogSubject(r.cluster, "sourcedelete"), r.configCrud.HandleSourcesDelete},
		{"catalog.sourcesync", bus.OperatorCatalogSubject(r.cluster, "sourcesync"), r.configCrud.HandleSourceSync},
		{"rbac.rolelist", bus.OperatorRbacSubject(r.cluster, "rolelist"), r.configCrud.HandleRolesList},
		{"rbac.roleget", bus.OperatorRbacSubject(r.cluster, "roleget"), r.configCrud.HandleRolesGet},
		{"rbac.rolecreate", bus.OperatorRbacSubject(r.cluster, "rolecreate"), r.configCrud.HandleRolesCreate},
		{"rbac.roleupdate", bus.OperatorRbacSubject(r.cluster, "roleupdate"), r.configCrud.HandleRolesUpdate},
		{"rbac.roledelete", bus.OperatorRbacSubject(r.cluster, "roledelete"), r.configCrud.HandleRolesDelete},
		{"rbac.bindinglist", bus.OperatorRbacSubject(r.cluster, "bindinglist"), r.configCrud.HandleBindingsList},
		{"rbac.bindingget", bus.OperatorRbacSubject(r.cluster, "bindingget"), r.configCrud.HandleBindingsGet},
		{"rbac.bindingcreate", bus.OperatorRbacSubject(r.cluster, "bindingcreate"), r.configCrud.HandleBindingsCreate},
		{"rbac.bindingupdate", bus.OperatorRbacSubject(r.cluster, "bindingupdate"), r.configCrud.HandleBindingsUpdate},
		{"rbac.bindingdelete", bus.OperatorRbacSubject(r.cluster, "bindingdelete"), r.configCrud.HandleBindingsDelete},
		{"provisioningdefaults.get", bus.OperatorProvisioningDefaultsSubject(r.cluster, "get"), r.configCrud.HandleProvisioningDefaultsGet},
		{"provisioningdefaults.update", bus.OperatorProvisioningDefaultsSubject(r.cluster, "update"), r.configCrud.HandleProvisioningDefaultsUpdate},
		{"versionprofiles.list", bus.OperatorVersionProfilesSubject(r.cluster, "list"), r.configCrud.HandleVersionProfilesList},
		{"versionprofiles.get", bus.OperatorVersionProfilesSubject(r.cluster, "get"), r.configCrud.HandleVersionProfilesGet},
		{"versionprofiles.create", bus.OperatorVersionProfilesSubject(r.cluster, "create"), r.configCrud.HandleVersionProfilesCreate},
		{"versionprofiles.update", bus.OperatorVersionProfilesSubject(r.cluster, "update"), r.configCrud.HandleVersionProfilesUpdate},
		{"versionprofiles.delete", bus.OperatorVersionProfilesSubject(r.cluster, "delete"), r.configCrud.HandleVersionProfilesDelete},
		{"versionprofiles.view", bus.OperatorVersionProfilesSubject(r.cluster, "view"), r.configCrud.HandleVersionProfilesView},
		// Namespace discovery (F4 add-cluster-namespace-discovery).
		{"namespaces.list", bus.OperatorNamespacesSubject(r.cluster), r.crud.HandleNamespacesList},
	}

	for _, sp := range subscriptions {
		sub, err := r.busConn.SubscribeRequest(sp.subject, operatorQueue, sp.handler)
		if err != nil {
			return fmt.Errorf("subscribe %s: %w", sp.name, err)
		}
		defer func(name string) {
			if err := sub.Unsubscribe(); err != nil {
				r.logger.Warn("unsubscribe "+name+" failed", "error", err)
			}
		}(sp.name)
	}

	// Hibernation wake: the BFF publishes at-most-once on wake.<cluster>.<ns>.<ctrl>
	// (nil payload; identity is in the subject). It does not fit the request/reply
	// subscription table above — it is a wildcard fire-and-forget subject whose
	// handler needs msg.Subject to recover ns/name. Queue-subscribed so a single
	// leader replica services each wake.
	wakeHibernateSub, err := r.busConn.QueueSubscribe(bus.WakeSubjectWildcard(r.cluster), operatorQueue,
		func(msg *nats.Msg) {
			ns, name, ok := bus.ParseWakeSubject(msg.Subject)
			if !ok {
				r.logger.Warn("ignoring malformed wake subject", "subject", msg.Subject)
				return
			}
			r.reconciler.WakeHibernatedController(context.Background(), ns, name)
		})
	if err != nil {
		return fmt.Errorf("subscribe hibernation wake: %w", err)
	}
	defer func() {
		if err := wakeHibernateSub.Unsubscribe(); err != nil {
			r.logger.Warn("unsubscribe hibernation wake failed", "error", err)
		}
	}()

	r.logger.Info("operator command subscribers registered (leader)")

	// Webhook replay: drain queued webhooks for hibernated-then-woken controllers
	// and deliver them to the mite. Leader-gated (this runnable only starts on the
	// leader); the durable JetStream consumer survives failover. Runs until ctx is
	// cancelled (leadership lost / shutdown).
	replayDone := make(chan struct{})
	go func() {
		defer close(replayDone)
		if err := r.reconciler.RunWebhookReplay(ctx, r.busConn, r.cluster, r.busConn.Replicas()); err != nil && ctx.Err() == nil {
			r.logger.Error("webhook replay consumer exited", "error", err)
		}
	}()

	<-ctx.Done()
	<-replayDone
	r.logger.Info("operator command subscribers unregistered (lost leadership)")
	return nil
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Default().Warn("envInt: parse error, using fallback", "key", key, "value", v, "error", err)
		return fallback
	}
	return n
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		slog.Default().Warn("envBool: parse error, using fallback", "key", key, "value", v, "error", err)
		return fallback
	}
	return b
}

type controllerStoreLister struct {
	store cache.Store
}

type provisioningDefaultsSetter interface {
	SetProvisioningDefaults(defaults *v1alpha1.ProvisioningDefaults)
}

func primeWakeRootDomain(ctx context.Context, store crdstore.Backend, setter provisioningDefaultsSetter, logger *slog.Logger) {
	d, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, store, "varroa-defaults", "")
	if err != nil {
		logger.Warn("initial provisioning defaults fetch for wake server", "error", err)
		return
	}
	if d != nil {
		setter.SetProvisioningDefaults(d)
	}
}

func (l controllerStoreLister) ListControllers() []*v1alpha1.Controller {
	items := l.store.List()
	controllers := make([]*v1alpha1.Controller, 0, len(items))
	for _, item := range items {
		if cr, ok := item.(*v1alpha1.Controller); ok {
			controllers = append(controllers, cr)
		}
	}
	return controllers
}

// reconcileAllVersionProfiles lists all JenkinsVersionProfile CRDs and
// reconciles each one via the profile reconciler.
func reconcileAllVersionProfiles(ctx context.Context, store crdstore.Backend, rec *controller.JenkinsVersionProfileReconciler, logger *slog.Logger) {
	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, store, "", "")
	if err != nil {
		logger.Warn("failed to list JenkinsVersionProfiles", "error", err)
		return
	}
	for _, p := range profiles {
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: p.Name}}
		if _, err := rec.Reconcile(ctx, req); err != nil {
			logger.Warn("failed to reconcile JenkinsVersionProfile", "profile", p.Name, "error", err)
		}
	}
}

// reconcileAllTeams lists all Team CRDs and reconciles each one via the
// team reconciler.
func reconcileAllTeams(ctx context.Context, store crdstore.Backend, rec *controller.TeamReconciler, logger *slog.Logger) {
	teams, err := crdstore.List[v1alpha1.Team](ctx, store, "", "")
	if err != nil {
		logger.Warn("failed to list Teams", "error", err)
		return
	}
	for _, t := range teams {
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: t.Name}}
		if _, err := rec.Reconcile(ctx, req); err != nil {
			logger.Warn("failed to reconcile Team", "team", t.Name, "error", err)
		}
	}
}

// storeItemLookupAdapter adapts crdstore.Backend to bundle.ItemLookup.
type storeItemLookupAdapter struct {
	store crdstore.Backend
}

func (a storeItemLookupAdapter) GetCatalogItemCRD(ctx context.Context, name, namespace string) (*v1alpha1.CatalogItem, error) {
	return crdstore.Get[v1alpha1.CatalogItem](ctx, a.store, name, namespace)
}
