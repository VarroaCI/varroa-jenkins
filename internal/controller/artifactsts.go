package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/overlay"
)

// Artifact STS duration bounds. AWS refuses an AssumeRole duration under
// 15 minutes or over 12 hours, and a role's MaxSessionDuration defaults to
// one hour.
const (
	DefaultArtifactSTSDuration = time.Hour
	MinArtifactSTSDuration     = 15 * time.Minute
	MaxArtifactSTSDuration     = 12 * time.Hour
)

// Keys of the projected copy in STS mode. The Jenkins JVM reads the
// credentials key as an AWS shared credentials file. The operator alone
// reads the two time keys to decide when to renew.
const (
	artifactCredentialsKey = "credentials"
	artifactIssuedAtKey    = "issued-at"
	artifactExpirationKey  = "expiration"
)

// artifactSharedCredentialsProperty is the AWS SDK for Java v2 system
// property that names the shared credentials file. It is a JVM property and
// not an environment variable, so a process that a build starts does not
// inherit it.
const artifactSharedCredentialsProperty = "-Daws.sharedCredentialsFile=" + artifactSecretMountPath + "/" + artifactCredentialsKey

// ReasonArtifactIssueFailed is the ArtifactStorage reason when the operator
// cannot issue or renew a cell's STS credential.
const ReasonArtifactIssueFailed = "IssueFailed"

// errArtifactIssuerMissing marks an issuer Secret that is absent or that
// holds no key pair. It is a configuration gap, not a read fault.
var errArtifactIssuerMissing = errors.New("artifact issuer secret missing")

// ArtifactSTSConfig is the operator's STS issuing configuration. A zero
// value disables STS mode.
type ArtifactSTSConfig struct {
	// IssuerSecret names a Secret in the operator namespace. It holds the
	// access-key-id and secret-access-key of the issuing identity.
	IssuerSecret string
	// RoleARN is the role the issuing identity assumes.
	RoleARN string
	// Endpoint overrides the STS endpoint URL. Empty selects the blob URL's
	// own endpoint, or the AWS regional endpoint when that is empty too.
	Endpoint string
	// Duration is the lifetime the operator requests for each credential.
	Duration time.Duration
}

// Enabled reports whether STS mode is configured.
func (c ArtifactSTSConfig) Enabled() bool { return c.IssuerSecret != "" }

// ParseArtifactSTSConfig validates the four VARROA_ARTIFACT_STS_* values.
// An empty issuer Secret disables STS mode and ignores the other values.
func ParseArtifactSTSConfig(issuerSecret, roleARN, endpoint, duration string) (ArtifactSTSConfig, error) {
	issuerSecret = strings.TrimSpace(issuerSecret)
	if issuerSecret == "" {
		return ArtifactSTSConfig{}, nil
	}
	cfg := ArtifactSTSConfig{
		IssuerSecret: issuerSecret,
		RoleARN:      strings.TrimSpace(roleARN),
		Endpoint:     strings.TrimSpace(endpoint),
		Duration:     DefaultArtifactSTSDuration,
	}
	if cfg.RoleARN == "" {
		return ArtifactSTSConfig{}, errors.New("VARROA_ARTIFACT_STS_ROLE_ARN is required when VARROA_ARTIFACT_STS_ISSUER_SECRET is set")
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return ArtifactSTSConfig{}, fmt.Errorf("VARROA_ARTIFACT_STS_ENDPOINT %q is not an http or https URL", cfg.Endpoint)
		}
	}
	if d := strings.TrimSpace(duration); d != "" {
		parsed, err := time.ParseDuration(d)
		if err != nil {
			return ArtifactSTSConfig{}, fmt.Errorf("VARROA_ARTIFACT_STS_DURATION %q: %w", d, err)
		}
		cfg.Duration = parsed
	}
	if cfg.Duration < MinArtifactSTSDuration || cfg.Duration > MaxArtifactSTSDuration {
		return ArtifactSTSConfig{}, fmt.Errorf("VARROA_ARTIFACT_STS_DURATION %s is outside %s to %s", cfg.Duration, MinArtifactSTSDuration, MaxArtifactSTSDuration)
	}
	return cfg, nil
}

// SetArtifactSTS sets the STS issuing configuration. A zero config keeps
// the static Secret path.
func (r *Reconciler) SetArtifactSTS(cfg ArtifactSTSConfig) {
	r.artifactSTS = cfg
}

// artifactIssueRequest is one AssumeRole call.
type artifactIssueRequest struct {
	AccessKeyID     string
	SecretAccessKey string
	Endpoint        string
	Region          string
	RoleARN         string
	SessionName     string
	Policy          string
	Duration        time.Duration
}

// artifactSessionCredential is the temporary credential STS returns.
type artifactSessionCredential struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// artifactIssuer issues one temporary credential. The seam lets tests run
// without an STS endpoint.
type artifactIssuer interface {
	Issue(ctx context.Context, req artifactIssueRequest) (artifactSessionCredential, error)
}

// stsArtifactIssuer calls STS AssumeRole through the AWS SDK for Go v2.
type stsArtifactIssuer struct {
	httpClient *http.Client
}

// Issue calls AssumeRole with the issuer's key pair and the inline session
// policy in req.
func (s stsArtifactIssuer) Issue(ctx context.Context, req artifactIssueRequest) (artifactSessionCredential, error) {
	opts := sts.Options{
		Region:      req.Region,
		Credentials: credentials.NewStaticCredentialsProvider(req.AccessKeyID, req.SecretAccessKey, ""),
	}
	if req.Endpoint != "" {
		opts.BaseEndpoint = aws.String(req.Endpoint)
	}
	if s.httpClient != nil {
		opts.HTTPClient = s.httpClient
	}
	out, err := sts.New(opts).AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(req.RoleARN),
		RoleSessionName: aws.String(req.SessionName),
		Policy:          aws.String(req.Policy),
		DurationSeconds: aws.Int32(int32(req.Duration / time.Second)),
	})
	if err != nil {
		return artifactSessionCredential{}, err
	}
	if out.Credentials == nil || aws.ToString(out.Credentials.SessionToken) == "" {
		return artifactSessionCredential{}, errors.New("AssumeRole returned no session credential")
	}
	return artifactSessionCredential{
		AccessKeyID:     aws.ToString(out.Credentials.AccessKeyId),
		SecretAccessKey: aws.ToString(out.Credentials.SecretAccessKey),
		SessionToken:    aws.ToString(out.Credentials.SessionToken),
		Expiration:      aws.ToTime(out.Credentials.Expiration),
	}, nil
}

// artifactPolicyDocument is an IAM session policy document.
type artifactPolicyDocument struct {
	Version   string                    `json:"Version"`
	Statement []artifactPolicyStatement `json:"Statement"`
}

type artifactPolicyStatement struct {
	Effect    string                       `json:"Effect"`
	Action    []string                     `json:"Action"`
	Resource  []string                     `json:"Resource"`
	Condition map[string]map[string]string `json:"Condition,omitempty"`
}

// artifactSessionPolicy renders the inline session policy for one prefix.
// It is the per-cell bucket policy of the docs: a prefix-conditioned
// ListBucket, GetBucketLocation, and object access under the prefix only.
// The effective grant is the intersection of this policy and the role's
// own policy.
func artifactSessionPolicy(bucket, prefix string) string {
	doc := artifactPolicyDocument{
		Version: "2012-10-17",
		Statement: []artifactPolicyStatement{
			{
				Effect:    "Allow",
				Action:    []string{"s3:ListBucket"},
				Resource:  []string{"arn:aws:s3:::" + bucket},
				Condition: map[string]map[string]string{"StringLike": {"s3:prefix": prefix + "*"}},
			},
			{
				Effect:   "Allow",
				Action:   []string{"s3:GetBucketLocation"},
				Resource: []string{"arn:aws:s3:::" + bucket},
			},
			{
				Effect:   "Allow",
				Action:   []string{"s3:PutObject", "s3:GetObject", "s3:DeleteObject"},
				Resource: []string{"arn:aws:s3:::" + bucket + "/" + prefix + "*"},
			},
		},
	}
	out, _ := json.Marshal(doc)
	return string(out)
}

var artifactSessionNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9+=,.@-]`)

// artifactSessionName renders the RoleSessionName for one controller. STS
// accepts at most 64 characters from [A-Za-z0-9+=,.@-]. A longer name keeps
// its first 55 characters and a hash suffix, so two cells never share one.
func artifactSessionName(cluster, namespace, name string) string {
	s := artifactSessionNameUnsafe.ReplaceAllString("varroa-"+cluster+"-"+namespace+"-"+name, "-")
	if len(s) <= 64 {
		return s
	}
	return s[:55] + "-" + sha256Hex([]byte(s))[:8]
}

// renderSharedCredentials renders an AWS shared credentials file with one
// default profile.
func renderSharedCredentials(c artifactSessionCredential) []byte {
	return []byte(fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\naws_session_token = %s\n",
		c.AccessKeyID, c.SecretAccessKey, c.SessionToken))
}

// artifactSTSEndpoint returns the STS endpoint URL for p. An explicit
// endpoint wins. Otherwise a custom S3 endpoint also serves STS, as MinIO
// and Ceph RGW do. An empty answer selects the AWS regional endpoint.
func (r *Reconciler) artifactSTSEndpoint(p blobstoreParams) string {
	if r.artifactSTS.Endpoint != "" {
		return r.artifactSTS.Endpoint
	}
	if p.CustomEndpoint == "" {
		return ""
	}
	scheme := "https"
	if p.UseHTTP {
		scheme = "http"
	}
	return scheme + "://" + p.CustomEndpoint
}

// readArtifactIssuer reads the issuer key pair from the operator namespace.
// An absent Secret, or one without both keys, wraps
// errArtifactIssuerMissing. Any other read error returns as it is.
func (r *Reconciler) readArtifactIssuer(ctx context.Context) (accessKeyID, secretAccessKey string, err error) {
	data, err := r.client.GetSecret(ctx, r.artifactSTS.IssuerSecret, r.getOperatorNamespace())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("%w: artifact issuer secret %q not found", errArtifactIssuerMissing, r.artifactSTS.IssuerSecret)
		}
		return "", "", err
	}
	ak, sk := string(data["access-key-id"]), string(data["secret-access-key"])
	if ak == "" || sk == "" {
		return "", "", fmt.Errorf("%w: artifact issuer secret %q has no access-key-id and secret-access-key", errArtifactIssuerMissing, r.artifactSTS.IssuerSecret)
	}
	return ak, sk, nil
}

// artifactNowFunc returns the reconciler's clock.
func (r *Reconciler) artifactNowFunc() time.Time {
	if r.artifactNow != nil {
		return r.artifactNow()
	}
	return time.Now()
}

// artifactCredentialFresh reports whether a projected STS copy needs no
// renewal yet. A copy is fresh until half of its own lifetime has passed.
// The lifetime comes from the copy, not from the configured duration,
// because a store can shorten the duration it grants.
func (r *Reconciler) artifactCredentialFresh(data map[string][]byte) bool {
	if len(data[artifactCredentialsKey]) == 0 {
		return false
	}
	issued, err1 := time.Parse(time.RFC3339, string(data[artifactIssuedAtKey]))
	expires, err2 := time.Parse(time.RFC3339, string(data[artifactExpirationKey]))
	if err1 != nil || err2 != nil || !expires.After(issued) {
		return false
	}
	renewAt := issued.Add(expires.Sub(issued) / 2)
	return r.artifactNowFunc().Before(renewAt)
}

// artifactIssueRetryInterval is the shortest time between two issue
// attempts for one cell after a failure. Without it, an STS outage would
// get one call per cell on every reconcile tick.
const artifactIssueRetryInterval = time.Minute

// artifactIssueFailures records the last issue failure for each controller,
// keyed "<namespace>/<name>". The fragment reads it to report IssueFailed.
// It lives in memory: after a restart the next renewal attempt sets it
// again. An entry of a deleted controller stays until the process exits;
// one entry is a short string and a time.
type artifactIssueFailures struct {
	m sync.Map
}

type artifactIssueFailure struct {
	message string
	at      time.Time
}

func (f *artifactIssueFailures) record(cr *v1alpha1.Controller, err error, at time.Time) {
	f.m.Store(cr.Namespace+"/"+cr.Name, artifactIssueFailure{message: err.Error(), at: at})
}

func (f *artifactIssueFailures) clear(cr *v1alpha1.Controller) {
	f.m.Delete(cr.Namespace + "/" + cr.Name)
}

func (f *artifactIssueFailures) load(cr *v1alpha1.Controller) (artifactIssueFailure, bool) {
	v, ok := f.m.Load(cr.Namespace + "/" + cr.Name)
	if !ok {
		return artifactIssueFailure{}, false
	}
	return v.(artifactIssueFailure), true
}

func (f *artifactIssueFailures) get(cr *v1alpha1.Controller) (string, bool) {
	failure, ok := f.load(cr)
	return failure.message, ok
}

// ensureArtifactSTSCredential keeps the projected copy of a cell's STS
// credential current. It reads the copy, and when the copy is fresh it
// clears any recorded failure and reads nothing more. When the copy is not
// fresh it reads the issuer key pair, issues a new credential, and rewrites
// the copy. A rewrite changes only the Secret data: the kubelet updates the
// mounted file in place, and the AWS SDK in Jenkins reads the new file on
// its next request. The pod does not restart and the operator applies no
// JCasC.
func (r *Reconciler) ensureArtifactSTSCredential(ctx context.Context, cr *v1alpha1.Controller, params blobstoreParams) (string, error) {
	name := cr.Name + "-artifacts"
	current, err := r.client.GetSecret(ctx, name, cr.Namespace)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("read artifact credential copy %q: %w", name, err)
	}
	if r.artifactCredentialFresh(current) {
		r.artifactFailures.clear(cr)
		return name, nil
	}
	if last, failed := r.artifactFailures.load(cr); failed && r.artifactNowFunc().Sub(last.at) < artifactIssueRetryInterval {
		return "", fmt.Errorf("issue artifact credential: retry after %s: %s", artifactIssueRetryInterval, last.message)
	}
	accessKeyID, secretAccessKey, err := r.readArtifactIssuer(ctx)
	if err != nil {
		r.artifactFailures.record(cr, err, r.artifactNowFunc())
		return "", err
	}
	issuer := r.artifactIssuer
	if issuer == nil {
		issuer = stsArtifactIssuer{}
	}
	region := params.Region
	if region == "" {
		region = "us-east-1"
	}
	prefix := artifactManagerPrefix(r.clusterName, cr.Namespace, cr.Name)
	issuedAt := r.artifactNowFunc().UTC()
	cred, err := issuer.Issue(ctx, artifactIssueRequest{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		Endpoint:        r.artifactSTSEndpoint(params),
		Region:          region,
		RoleARN:         r.artifactSTS.RoleARN,
		SessionName:     artifactSessionName(r.clusterName, cr.Namespace, cr.Name),
		Policy:          artifactSessionPolicy(params.Container, prefix),
		Duration:        r.artifactSTS.Duration,
	})
	if err != nil {
		issueErr := fmt.Errorf("issue artifact credential: %w", err)
		r.artifactFailures.record(cr, issueErr, r.artifactNowFunc())
		return "", issueErr
	}
	data := map[string][]byte{
		artifactCredentialsKey: renderSharedCredentials(cred),
		artifactIssuedAtKey:    []byte(issuedAt.Format(time.RFC3339)),
		artifactExpirationKey:  []byte(cred.Expiration.UTC().Format(time.RFC3339)),
	}
	if err := r.writeArtifactCopy(ctx, cr, name, data); err != nil {
		return "", fmt.Errorf("project artifact credential %q: %w", name, err)
	}
	// A credential that is not fresh at once, for example because the
	// store's clock trails the operator's, would be issued again on every
	// tick. Record it as a failure so the retry interval applies.
	if !r.artifactCredentialFresh(data) {
		r.artifactFailures.record(cr, fmt.Errorf("issued artifact credential expires at %s, before half of its lifetime can pass", cred.Expiration.UTC().Format(time.RFC3339)), r.artifactNowFunc())
		return name, nil
	}
	r.artifactFailures.clear(cr)
	return name, nil
}

// writeArtifactCopy writes the projected artifact Secret with an owner
// reference to cr when the backend supports one.
func (r *Reconciler) writeArtifactCopy(ctx context.Context, cr *v1alpha1.Controller, name string, data map[string][]byte) error {
	if setter, ok := r.client.(secretWithOwnerCreator); ok {
		return setter.CreateOrUpdateSecretWithOwner(ctx, name, cr.Namespace, data, controllerOwnerRef(cr))
	}
	return r.client.CreateOrUpdateSecret(ctx, name, cr.Namespace, data)
}

// resolveArtifactSTSSecret is resolveArtifactSecret in STS mode. It issues
// only for a cell on an s3 blob tier whose issuer Secret exists. Any other
// controller gets no projection, which is the LocalOnly case. The data hash
// is always empty, so a renewal never changes the desired-state hash.
func (r *Reconciler) resolveArtifactSTSSecret(ctx context.Context, cr *v1alpha1.Controller) (string, error) {
	if !v1alpha1.IsCell(cr) {
		return "", nil
	}
	params, parsed := parseBlobstoreURL(r.blobstoreURL)
	if !parsed || !params.IsS3 {
		return "", nil
	}
	if _, _, err := r.readArtifactIssuer(ctx); err != nil {
		if errors.Is(err, errArtifactIssuerMissing) {
			return "", nil
		}
		return "", fmt.Errorf("read artifact issuer secret %q: %w", r.artifactSTS.IssuerSecret, err)
	}
	return r.ensureArtifactSTSCredential(ctx, cr, params)
}

// renewArtifactCredential runs on every Running and Connected tick in STS
// mode. It renews a cell's credential once half of its lifetime has
// passed. A failure keeps the current copy, which stays valid until it
// expires, and records the error so the ArtifactStorage condition reports
// IssueFailed on the same tick.
func (r *Reconciler) renewArtifactCredential(ctx context.Context, cr *v1alpha1.Controller) {
	if !r.artifactSTS.Enabled() || !v1alpha1.IsCell(cr) {
		return
	}
	params, parsed := parseBlobstoreURL(r.blobstoreURL)
	if !parsed || !params.IsS3 {
		return
	}
	if _, err := r.ensureArtifactSTSCredential(ctx, cr, params); err != nil && r.Logger != nil {
		r.Logger.Warn("artifact credential renewal failed", "controller", cr.Namespace+"/"+cr.Name, "error", err)
	}
}

// reconcileArtifactSTSRoll moves a Connected cell to Provisioning when its
// live Jenkins container lacks the shared credentials property. That
// happens once, after an install moves from static mode to STS mode: the
// JVM reads the property only at start.
func (r *Reconciler) reconcileArtifactSTSRoll(ctx context.Context, cr *v1alpha1.Controller) bool {
	if !v1alpha1.IsCell(cr) {
		return false
	}
	// Roll only a cell whose copy already holds a fresh STS credential.
	// renewArtifactCredential runs earlier on the same tick, so a fresh copy
	// proves that an issue works. A cell without one would get a pod without
	// the property too, or would block in Provisioning on a failing issue.
	current, err := r.client.GetSecret(ctx, cr.Name+"-artifacts", cr.Namespace)
	if err != nil && !apierrors.IsNotFound(err) {
		if r.Logger != nil {
			r.Logger.Warn("artifact-sts-roll evaluation skipped", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		}
		return false
	}
	if !r.artifactCredentialFresh(current) {
		return false
	}
	env, found, err := r.client.GetStatefulSetContainerEnv(ctx, controllerPrefix(cr), cr.Namespace, overlay.JenkinsContainerName)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("artifact-sts-roll evaluation skipped", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		}
		return false
	}
	if !found || strings.Contains(env["JAVA_OPTS"], artifactSharedCredentialsProperty) {
		return false
	}
	now := metav1Now()
	cr.Status.Phase = v1alpha1.ControllerPhaseProvisioning
	cr.Status.ProvisioningStartedAt = &now
	cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
		Type:               v1alpha1.ConditionProvisioning,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "ProvisioningStarted",
		Message:            "re-provisioning to read STS artifact credentials",
	})
	return true
}

// artifactSTSFragment is artifactStorageFragment in STS mode. It emits the
// fragment for a cell whose issuer Secret exists and whose plugin set pins
// artifact-manager-s3. A recorded issue failure keeps the fragment, so the
// running Jenkins keeps its artifact manager, but turns the condition to
// False/IssueFailed. That condition makes CellReady false, which stops new
// placements and never rolls the pod.
func (r *Reconciler) artifactSTSFragment(ctx context.Context, cr *v1alpha1.Controller, params blobstoreParams) (string, v1alpha1.ControllerCondition, error) {
	if !v1alpha1.IsCell(cr) {
		return "", artifactStorageCondition(metav1.ConditionFalse, "LocalOnly",
			"the operator issues STS artifact credentials to cells only"), nil
	}
	if _, _, err := r.readArtifactIssuer(ctx); err != nil {
		if errors.Is(err, errArtifactIssuerMissing) {
			return "", artifactStorageCondition(metav1.ConditionFalse, "LocalOnly",
				strings.TrimPrefix(err.Error(), errArtifactIssuerMissing.Error()+": ")), nil
		}
		cond := artifactStorageCondition(metav1.ConditionFalse, "LocalOnly",
			fmt.Sprintf("artifact issuer secret %q read failed: %v", r.artifactSTS.IssuerSecret, err))
		return "", cond, fmt.Errorf("%w: secret %q: %v", errArtifactSecretRead, r.artifactSTS.IssuerSecret, err)
	}
	coreSet := r.coreSetForCr(ctx, cr, r.Logger)
	if !pluginPinned(coreSet, "artifact-manager-s3") {
		return "", artifactStorageCondition(metav1.ConditionFalse, "PluginMissing",
			"resolved plugin set does not pin artifact-manager-s3"), nil
	}
	fragment, err := renderArtifactStorageFragment(params, r.clusterName, cr.Namespace, cr.Name, true)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("artifact storage: failed to render fragment", "controller", cr.Namespace+"/"+cr.Name, "error", err)
		}
		return "", artifactStorageCondition(metav1.ConditionFalse, "LocalOnly", "failed to render artifact-storage fragment"), nil
	}
	if msg, failed := r.artifactFailures.get(cr); failed {
		return fragment, artifactStorageCondition(metav1.ConditionFalse, ReasonArtifactIssueFailed, msg), nil
	}
	prefix := artifactManagerPrefix(r.clusterName, cr.Namespace, cr.Name)
	return fragment, artifactStorageCondition(metav1.ConditionTrue, "External",
		fmt.Sprintf("artifacts write to bucket %q under prefix %q with STS credentials from issuer Secret %q",
			params.Container, prefix, r.artifactSTS.IssuerSecret)), nil
}
