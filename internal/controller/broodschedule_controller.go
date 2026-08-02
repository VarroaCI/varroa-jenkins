package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth/schedule"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
)

const (
	scheduleFinalizer   = "varroa.dev/broodschedule-rbac"
	scheduleRequeue     = 15 * time.Minute
	tokenSecretSuffix   = "-schedule-token"
	bindingAnnNamespace = "varroa.dev/broodschedule-namespace"
	bindingAnnName      = "varroa.dev/broodschedule-name"
	// These bound each fired Kubernetes Job; the BroodOperation has its own
	// TTL and garbage-collection policy, which is independent of Job cleanup.
	scheduleJobActiveDeadlineSeconds int64 = 3600
	scheduleJobTTLSeconds            int32 = 3600
	scheduleOpDefaultTTLSeconds      int32 = 86400
)

// BroodScheduleReconciler reconciles BroodSchedule CRDs.
type BroodScheduleReconciler struct {
	client            client.Client
	apiReader         client.Reader
	scheme            *runtime.Scheme
	operatorNamespace string
	signer            *mitesrv.MiteTokenSigner
	logger            *slog.Logger
}

// NewBroodScheduleReconciler creates a new BroodScheduleReconciler.
func NewBroodScheduleReconciler(
	client client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	operatorNamespace string,
	signer *mitesrv.MiteTokenSigner,
	logger *slog.Logger,
) *BroodScheduleReconciler {
	return &BroodScheduleReconciler{
		client:            client,
		apiReader:         apiReader,
		scheme:            scheme,
		operatorNamespace: operatorNamespace,
		signer:            signer,
		logger:            logger,
	}
}

// SetupWithManager registers this reconciler with a controller-runtime Manager.
func (r *BroodScheduleReconciler) SetupWithManager(mgr manager.Manager) error {
	return builder.ControllerManagedBy(mgr).
		For(&v1alpha1.BroodSchedule{}).
		Owns(&batchv1.CronJob{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

// Reconcile implements reconcile.Reconciler.
func (r *BroodScheduleReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("broodschedule", req.Namespace+"/"+req.Name)

	var cr v1alpha1.BroodSchedule
	if err := r.client.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Step 1: Handle deletion (finalizer-based cleanup).
	if !cr.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &cr)
	}

	// Step 2: Ensure finalizer.
	if !controllerutil.ContainsFinalizer(&cr, scheduleFinalizer) {
		controllerutil.AddFinalizer(&cr, scheduleFinalizer)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var fresh v1alpha1.BroodSchedule
			if err := r.client.Get(ctx, client.ObjectKeyFromObject(&cr), &fresh); err != nil {
				return err
			}
			if !controllerutil.AddFinalizer(&fresh, scheduleFinalizer) {
				return nil
			}
			return r.client.Update(ctx, &fresh)
		}); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{Requeue: true}, nil
	}

	// Step 3: Tenancy check (task 3.9) — run before mint/binding/cronjob.
	tenancyViolated := false
	if err := ValidateBroodTenancy(
		v1alpha1.BroodOperationSpec{Targets: cr.Spec.Template.Targets},
		cr.Namespace,
		r.operatorNamespace,
	); err != nil {
		tenancyViolated = true
	}
	// Schedule-specific: at-most-one cluster in team-namespace mode.
	if cr.Namespace != r.operatorNamespace && len(cr.Spec.Template.Clusters) > 1 {
		tenancyViolated = true
	}

	if tenancyViolated {
		cr.Status.Reason = "TenancyViolation"
	} else {
		cr.Status.Reason = "" // clear on recovery
	}

	// Step 4: Mint/rotate token Secret (task 3.2).
	if r.signer == nil {
		logger.Warn("no signing key available, cannot mint schedule token")
		return reconcile.Result{RequeueAfter: scheduleRequeue}, nil
	}
	token, err := r.ensureTokenSecret(ctx, &cr)
	if err != nil {
		logger.Error("failed to ensure token secret", "error", err)
		return reconcile.Result{}, err
	}
	_ = token // token was written to Secret; Secret confirmation is in step 6

	// Step 5: Create/update VarroaRoleBinding (task 3.3).
	if err := r.ensureVarroaRoleBinding(ctx, &cr); err != nil {
		logger.Error("failed to ensure VarroaRoleBinding", "error", err)
		return reconcile.Result{}, err
	}

	// Step 6: Confirm ordering — both binding and Secret must be visible via
	// uncached read (task 3.4) before CronJob create/enable.
	bindingName := bindingNameFor(&cr)
	var binding v1alpha1.VarroaRoleBinding
	if err := r.apiReader.Get(ctx, types.NamespacedName{Name: bindingName}, &binding); err != nil {
		logger.Info("VarroaRoleBinding not yet visible via apiReader, delaying CronJob", "binding", bindingName)
		return reconcile.Result{RequeueAfter: scheduleRequeue}, nil //nolint:nilerr // requeue to retry; confirmation read lag is expected, not a hard error
	}
	var secret corev1.Secret
	if err := r.apiReader.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name + tokenSecretSuffix}, &secret); err != nil {
		logger.Info("token Secret not yet visible via apiReader, delaying CronJob", "secret", cr.Name+tokenSecretSuffix)
		return reconcile.Result{RequeueAfter: scheduleRequeue}, nil //nolint:nilerr // requeue to retry; confirmation read lag is expected, not a hard error
	}

	// Step 7: CronJob step (task 3.5).
	if err := r.ensureCronJob(ctx, &cr, tenancyViolated); err != nil {
		logger.Error("failed to ensure CronJob", "error", err)
		return reconcile.Result{}, err
	}

	// Step 8: Status (task 3.6).
	if err := r.updateStatus(ctx, &cr); err != nil {
		logger.Error("failed to update status", "error", err)
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: scheduleRequeue}, nil
}

// reconcileDelete handles deletion of a BroodSchedule.
func (r *BroodScheduleReconciler) reconcileDelete(ctx context.Context, cr *v1alpha1.BroodSchedule) (reconcile.Result, error) {
	if controllerutil.ContainsFinalizer(cr, scheduleFinalizer) {
		bindingName := bindingNameFor(cr)
		var binding v1alpha1.VarroaRoleBinding
		if err := r.client.Get(ctx, types.NamespacedName{Name: bindingName}, &binding); err == nil {
			if err := r.client.Delete(ctx, &binding); err != nil && !apierrors.IsNotFound(err) {
				return reconcile.Result{}, err
			}
		} else if !apierrors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var fresh v1alpha1.BroodSchedule
			if err := r.client.Get(ctx, client.ObjectKeyFromObject(cr), &fresh); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}
			if !controllerutil.RemoveFinalizer(&fresh, scheduleFinalizer) {
				return nil
			}
			return r.client.Update(ctx, &fresh)
		}); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

// ensureTokenSecret ensures the schedule token Secret exists and is fresh.
// Returns the token string (already written to the Secret).
func (r *BroodScheduleReconciler) ensureTokenSecret(ctx context.Context, cr *v1alpha1.BroodSchedule) (string, error) {
	secretName := cr.Name + tokenSecretSuffix
	ns := cr.Namespace

	// Check existing Secret.
	var existing corev1.Secret
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, &existing); err == nil {
		// Decode existing token to check expiry.
		if tokenData, ok := existing.Data["token"]; ok && len(tokenData) > 0 {
			parts := strings.Split(string(tokenData), ".")
			if len(parts) == 3 {
				payloadJSON, err := base64Decode(parts[1])
				if err == nil {
					var claims struct {
						Exp int64 `json:"exp"`
					}
					if json.Unmarshal(payloadJSON, &claims) == nil {
						// More than 30 minutes remaining? Keep it.
						if claims.Exp-time.Now().Unix() > 30*60 {
							return string(tokenData), nil
						}
					}
				}
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}

	// Mint new token.
	now := time.Now()
	claimsMap := map[string]interface{}{
		"sub": "schedule:" + cr.Namespace + "/" + cr.Name,
		"aud": schedule.Audience,
		"exp": now.Add(time.Hour).Unix(),
		"iat": now.Unix(),
	}
	token, err := r.signer.Signer().SignJWT(claimsMap)
	if err != nil {
		return "", fmt.Errorf("sign schedule token: %w", err)
	}

	// Upsert Secret.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: ns,
		},
		Data: map[string][]byte{
			"token": []byte(token),
		},
		Type: corev1.SecretTypeOpaque,
	}
	if err := controllerutil.SetControllerReference(cr, secret, r.scheme); err != nil {
		return "", err
	}

	if existing.Name == "" {
		return token, r.client.Create(ctx, secret)
	}
	existing.Data = secret.Data
	return token, r.client.Update(ctx, &existing)
}

// ensureVarroaRoleBinding creates or updates the cluster-scoped VarroaRoleBinding.
func (r *BroodScheduleReconciler) ensureVarroaRoleBinding(ctx context.Context, cr *v1alpha1.BroodSchedule) error {
	bindingName := bindingNameFor(cr)
	isOperatorNS := cr.Namespace == r.operatorNamespace

	roleRef := "operator"
	var scope *v1alpha1.VarroaRoleBindingScope
	if isOperatorNS {
		roleRef = "admin"
		scope = nil
	} else {
		scope = &v1alpha1.VarroaRoleBindingScope{
			Namespaces: []string{cr.Namespace},
		}
	}

	want := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: bindingName,
			Annotations: map[string]string{
				bindingAnnNamespace: cr.Namespace,
				bindingAnnName:      cr.Name,
			},
		},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{
				{Kind: "User", Name: "schedule:" + cr.Namespace + "/" + cr.Name},
			},
			RoleRef: roleRef,
			Scope:   scope,
		},
	}
	// No OwnerReference — cluster-scoped binding cannot be owned by namespaced BroodSchedule.

	var existing v1alpha1.VarroaRoleBinding
	if err := r.client.Get(ctx, types.NamespacedName{Name: bindingName}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return r.client.Create(ctx, want)
		}
		return err
	}

	// Update existing.
	existing.Annotations = want.Annotations
	existing.Spec = want.Spec
	return r.client.Update(ctx, &existing)
}

// ensureCronJob creates or updates the owned CronJob.
func (r *BroodScheduleReconciler) ensureCronJob(ctx context.Context, cr *v1alpha1.BroodSchedule, tenancyViolated bool) error {
	cronjobName := cr.Name
	ns := cr.Namespace

	suspend := cr.Spec.Suspend || tenancyViolated

	backoffLimit := int32(0)
	activeDeadlineSeconds := scheduleJobActiveDeadlineSeconds
	ttlSecondsAfterFinished := scheduleJobTTLSeconds
	automountSA := false

	desired := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cronjobName,
			Namespace: ns,
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   cr.Spec.Schedule,
			Suspend:                    &suspend,
			ConcurrencyPolicy:          cr.Spec.ConcurrencyPolicy,
			StartingDeadlineSeconds:    cr.Spec.StartingDeadlineSeconds,
			SuccessfulJobsHistoryLimit: cr.Spec.SuccessfulJobsHistoryLimit,
			FailedJobsHistoryLimit:     cr.Spec.FailedJobsHistoryLimit,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					BackoffLimit:            &backoffLimit,
					ActiveDeadlineSeconds:   &activeDeadlineSeconds,
					TTLSecondsAfterFinished: &ttlSecondsAfterFinished,
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								// Selected by the BFF NetworkPolicy so the fired Job can reach the BFF.
								"app.kubernetes.io/component": "brood-schedule",
							},
						},
						Spec: corev1.PodSpec{
							RestartPolicy:                corev1.RestartPolicyNever,
							AutomountServiceAccountToken: &automountSA,
							ImagePullSecrets:             scheduleImagePullSecrets(),
							Containers: []corev1.Container{
								{
									Name:    "varroactl",
									Image:   os.Getenv("VARROA_VARROACTL_IMAGE"),
									Command: buildCommand(cr),
									Env: []corev1.EnvVar{
										{
											Name:  "VARROACTL_SERVER",
											Value: scheduleBFFURL(r.operatorNamespace),
										},
										{
											Name: "VARROACTL_API_KEY",
											ValueFrom: &corev1.EnvVarSource{
												SecretKeyRef: &corev1.SecretKeySelector{
													LocalObjectReference: corev1.LocalObjectReference{
														Name: cr.Name + tokenSecretSuffix,
													},
													Key: "token",
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(cr, desired, r.scheme); err != nil {
		return err
	}

	// Check if tenancy violated and no existing CronJob — skip creation.
	var existing batchv1.CronJob
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: cronjobName}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			if tenancyViolated {
				return nil // skip creation
			}
			return r.client.Create(ctx, desired)
		}
		return err
	}

	// Update existing.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: cronjobName}, &existing); err != nil {
			return err
		}
		existing.Spec = desired.Spec
		return r.client.Update(ctx, &existing)
	})
}

// scheduleBFFURL returns the in-cluster BFF base URL that the fired Job's
// varroactl call targets. The Helm chart injects VARROA_SCHEDULE_BFF_URL with
// the release-prefixed BFF Service name; fall back to a best-effort default
// only when it is unset.
func scheduleBFFURL(operatorNamespace string) string {
	if v := strings.TrimSpace(os.Getenv("VARROA_SCHEDULE_BFF_URL")); v != "" {
		return v
	}
	return "http://varroa-bff." + operatorNamespace + ".svc:8080"
}

// scheduleImagePullSecrets returns the imagePullSecrets attached to the fired
// Job pod so it can pull the (private) varroactl image. Populated from the
// chart-injected VARROA_IMAGE_PULL_SECRETS (comma-separated Secret names).
func scheduleImagePullSecrets() []corev1.LocalObjectReference {
	raw := strings.TrimSpace(os.Getenv("VARROA_IMAGE_PULL_SECRETS"))
	if raw == "" {
		return nil
	}
	var refs []corev1.LocalObjectReference
	for _, n := range strings.Split(raw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			refs = append(refs, corev1.LocalObjectReference{Name: n})
		}
	}
	return refs
}

// buildCommand builds the command array for the CronJob's container.
func buildCommand(cr *v1alpha1.BroodSchedule) []string {
	// Absolute path: the image ships binaries in /app, which is not on $PATH
	// (all component Deployments invoke /app/<binary> the same way).
	cmd := []string{"/app/varroactl", "broodop", "run", string(cr.Spec.Template.Action.Verb)}

	// The BroodOperation is created in the schedule's own namespace. Without this
	// the BFF defaults to the operator namespace, which denies the operator-scoped
	// (non-admin) schedule identity in team-namespace mode.
	cmd = append(cmd, "--namespace="+cr.Namespace)

	t := cr.Spec.Template.Targets
	if len(t.Names) > 0 {
		cmd = append(cmd, "--names="+strings.Join(t.Names, ","))
	}
	if t.Selector != nil {
		b, _ := json.Marshal(t.Selector)
		cmd = append(cmd, "--selector-json="+string(b))
	}
	if len(t.Namespaces) > 0 {
		cmd = append(cmd, "--namespaces="+strings.Join(t.Namespaces, ","))
	}
	if len(cr.Spec.Template.Clusters) > 0 {
		cmd = append(cmd, "--clusters="+strings.Join(cr.Spec.Template.Clusters, ","))
	}
	if t.Filters != nil {
		if t.Filters.Phase != nil {
			cmd = append(cmd, "--filter=phase="+string(*t.Filters.Phase))
		}
		if t.Filters.Version != nil {
			cmd = append(cmd, "--filter=version="+*t.Filters.Version)
		}
		if t.Filters.Bundle != nil {
			cmd = append(cmd, "--filter=bundle="+*t.Filters.Bundle)
		}
	}

	if e := cr.Spec.Template.Execution; e != nil {
		if e.Order != "" {
			cmd = append(cmd, "--order="+string(e.Order))
		}
		if e.MaxParallel != nil {
			cmd = append(cmd, fmt.Sprintf("--max-parallel=%d", *e.MaxParallel))
		}
		if e.FailurePolicy != "" {
			cmd = append(cmd, "--failure-policy="+string(e.FailurePolicy))
		}
	}

	ttl := scheduleOpDefaultTTLSeconds
	if cr.Spec.Template.TTLSecondsAfterFinished != nil {
		ttl = *cr.Spec.Template.TTLSecondsAfterFinished
	}
	cmd = append(cmd, fmt.Sprintf("--ttl=%d", ttl))

	if cr.Spec.WaitForCompletion {
		cmd = append(cmd, "--watch")
	}

	return cmd
}

// updateStatus copies the owned CronJob's status into the BroodSchedule status.
// If the CronJob doesn't exist yet, it still persists the schedule's status fields
// (e.g. Reason) that were set earlier in the reconcile.
func (r *BroodScheduleReconciler) updateStatus(ctx context.Context, cr *v1alpha1.BroodSchedule) error {
	var cj batchv1.CronJob
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}, &cj); err != nil {
		if apierrors.IsNotFound(err) {
			// No CronJob yet — persist Reason etc. from what was set.
			desired := cr.Status.DeepCopy()
			return retry.RetryOnConflict(retry.DefaultRetry, func() error {
				var fresh v1alpha1.BroodSchedule
				if err := r.client.Get(ctx, client.ObjectKeyFromObject(cr), &fresh); err != nil {
					return err
				}
				fresh.Status = *desired.DeepCopy()
				return r.client.Status().Update(ctx, &fresh)
			})
		}
		return err
	}

	cr.Status.LastScheduleTime = cj.Status.LastScheduleTime
	cr.Status.LastSuccessfulTime = cj.Status.LastSuccessfulTime
	cr.Status.Active = cj.Status.Active
	desired := cr.Status.DeepCopy()

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh v1alpha1.BroodSchedule
		if err := r.client.Get(ctx, client.ObjectKeyFromObject(cr), &fresh); err != nil {
			return err
		}
		fresh.Status = *desired.DeepCopy()
		return r.client.Status().Update(ctx, &fresh)
	})
}

// bindingNameFor returns the deterministic hashed name for the VarroaRoleBinding.
func bindingNameFor(cr *v1alpha1.BroodSchedule) string {
	sum := sha256.Sum256([]byte(cr.Namespace + "/" + cr.Name))
	return "broodschedule-" + hex.EncodeToString(sum[:])[:48]
}

// base64Decode decodes a base64url-encoded string (JWT payload) without padding.
func base64Decode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}
