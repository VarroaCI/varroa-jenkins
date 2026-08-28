package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth/schedule"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
)

func init() {
	_ = v1alpha1.AddToScheme(scheme.Scheme)
	_ = batchv1.AddToScheme(scheme.Scheme)
	_ = corev1.AddToScheme(scheme.Scheme)
}

// newTestScheduleReconciler creates a reconciler backed by a fake client and a real signer.
func newTestScheduleReconciler(t *testing.T, objs ...client.Object) (*BroodScheduleReconciler, *mitesrv.MiteTokenSigner) {
	t.Helper()
	s := scheme.Scheme
	b := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(
		&v1alpha1.BroodSchedule{},
	)
	cl := b.Build()

	mts, err := mitesrv.NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}

	r := NewBroodScheduleReconciler(
		cl,
		cl, // apiReader uses same fake client (both are already consistent in fake mode)
		s,
		"operator-ns",
		mts,
		nil, // logger
	)
	return r, mts
}

// testSchedule creates a minimal BroodSchedule for tests.
func testSchedule(name, ns string) *v1alpha1.BroodSchedule {
	cs := &v1alpha1.BroodSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: v1alpha1.BroodScheduleSpec{
			Schedule: "*/5 * * * *",
			Template: v1alpha1.BroodScheduleTemplate{
				Targets: v1alpha1.BroodTargets{
					Names: []string{"ctrl-1"},
				},
				Action: v1alpha1.BroodAction{
					Verb: v1alpha1.BroodVerbReconcile,
				},
			},
			WaitForCompletion: true,
		},
	}
	return cs
}

// --- Task 3.2: Token mint and rotation ---

func TestScheduleReconciler_TokenMinted(t *testing.T) {
	cr := testSchedule("my-schedule", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	r, mts := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "my-schedule"}}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected non-zero RequeueAfter")
	}

	// Verify token Secret exists.
	var secret corev1.Secret
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "my-schedule" + tokenSecretSuffix}, &secret); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	token := string(secret.Data["token"])
	if token == "" {
		t.Fatal("token is empty")
	}

	// Decode claims.
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 JWT parts, got %d", len(parts))
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Sub string `json:"sub"`
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Sub != "schedule:team-ns/my-schedule" {
		t.Errorf("sub = %q, want %q", claims.Sub, "schedule:team-ns/my-schedule")
	}
	if claims.Aud != schedule.Audience {
		t.Errorf("aud = %q, want %q", claims.Aud, schedule.Audience)
	}
	if claims.Exp < time.Now().Unix()+3300 {
		t.Errorf("exp too soon: %d (now=%d)", claims.Exp, time.Now().Unix())
	}

	// Verify the verifier accepts it end-to-end.
	privPEM, err := mts.PrivateKeyPEM()
	if err != nil {
		t.Fatalf("PrivateKeyPEM: %v", err)
	}
	mts2, err := mitesrv.NewMiteTokenSignerFromPEM([]byte(privPEM))
	if err != nil {
		t.Fatalf("recreate signer: %v", err)
	}
	sv := schedule.NewVerifier(mts2.Signer())
	ac, matched, err := sv.Verify(context.Background(), token)
	if !matched {
		t.Errorf("verifier matched=false, want true")
	}
	if err != nil {
		t.Errorf("verifier err=%v, want nil", err)
	}
	if ac == nil || ac.Subject != "schedule:team-ns/my-schedule" {
		t.Errorf("claims subject = %v, want schedule:team-ns/my-schedule", ac)
	}
}

func TestScheduleReconciler_TokenRotation(t *testing.T) {
	cr := testSchedule("rot-sched", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	r, mts := newTestScheduleReconciler(t, cr)

	// First reconcile mints a token.
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "rot-sched"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	var secret corev1.Secret
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "rot-sched" + tokenSecretSuffix}, &secret); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	firstToken := string(secret.Data["token"])

	// Second reconcile immediately — token still fresh (>30min), should NOT rotate.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "rot-sched" + tokenSecretSuffix}, &secret); err != nil {
		t.Fatalf("get secret after second reconcile: %v", err)
	}
	secondToken := string(secret.Data["token"])
	if secondToken != firstToken {
		t.Errorf("token rotated when still fresh (should not happen)")
	}

	// Now simulate token that expires soon: replace with one expiring in 10 min.
	earlyExp := time.Now().Add(10 * time.Minute).Unix()
	claimsMap := map[string]interface{}{
		"sub": "schedule:team-ns/rot-sched",
		"aud": schedule.Audience,
		"exp": earlyExp,
		"iat": time.Now().Unix(),
	}
	expiringToken, err := mts.Signer().SignJWT(claimsMap)
	if err != nil {
		t.Fatalf("sign expiring token: %v", err)
	}
	secret.Data = map[string][]byte{"token": []byte(expiringToken)}
	if err := r.client.Update(context.Background(), &secret); err != nil {
		t.Fatalf("update secret with expiring token: %v", err)
	}

	// Reconcile again — should rotate now.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "rot-sched" + tokenSecretSuffix}, &secret); err != nil {
		t.Fatalf("get secret after third reconcile: %v", err)
	}
	thirdToken := string(secret.Data["token"])
	if thirdToken == expiringToken {
		t.Errorf("token was NOT rotated despite <30min validity")
	}
}

// --- Task 3.3: VarroaRoleBinding create/delete ---

func TestScheduleReconciler_RoleBindingCreated(t *testing.T) {
	cr := testSchedule("bnd-sched", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "bnd-sched"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	bindingName := bindingNameFor(cr)
	var binding v1alpha1.VarroaRoleBinding
	if err := r.client.Get(context.Background(), types.NamespacedName{Name: bindingName}, &binding); err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if binding.Spec.RoleRef != "operator" {
		t.Errorf("roleRef = %q, want operator", binding.Spec.RoleRef)
	}
	if len(binding.Spec.Subjects) != 1 || binding.Spec.Subjects[0].Name != "schedule:team-ns/bnd-sched" {
		t.Errorf("subjects = %+v", binding.Spec.Subjects)
	}
	if binding.Spec.Scope == nil || len(binding.Spec.Scope.Namespaces) != 1 || binding.Spec.Scope.Namespaces[0] != "team-ns" {
		t.Errorf("scope = %+v", binding.Spec.Scope)
	}
	if binding.Annotations[bindingAnnNamespace] != "team-ns" || binding.Annotations[bindingAnnName] != "bnd-sched" {
		t.Errorf("annotations = %+v", binding.Annotations)
	}
	// No owner reference.
	if len(binding.OwnerReferences) != 0 {
		t.Errorf("expected no owner references on cluster-scoped binding, got %d", len(binding.OwnerReferences))
	}
}

func TestScheduleReconciler_RoleBindingReconcileIdempotent(t *testing.T) {
	cr := testSchedule("idm-sched", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "idm-sched"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	// List bindings — there should be exactly one.
	var list v1alpha1.VarroaRoleBindingList
	if err := r.client.List(context.Background(), &list); err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected 1 binding, got %d", len(list.Items))
	}
}

func TestScheduleReconciler_RoleBindingDelete(t *testing.T) {
	cr := testSchedule("del-sched", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	r, _ := newTestScheduleReconciler(t, cr)

	// Create binding via reconcile.
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "del-sched"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	// Verify binding exists.
	bindingName := bindingNameFor(cr)
	var binding v1alpha1.VarroaRoleBinding
	if err := r.client.Get(context.Background(), types.NamespacedName{Name: bindingName}, &binding); err != nil {
		t.Fatalf("binding should exist: %v", err)
	}

	// Delete the schedule.
	if err := r.client.Delete(context.Background(), cr); err != nil {
		t.Fatalf("delete schedule: %v", err)
	}

	// Re-fetch to get deletion timestamp.
	var delCR v1alpha1.BroodSchedule
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "del-sched"}, &delCR); err != nil {
		t.Fatalf("get deleted schedule: %v", err)
	}

	// Reconcile deletion.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}

	// Binding should be gone.
	if err := r.client.Get(context.Background(), types.NamespacedName{Name: bindingName}, &binding); !apierrors.IsNotFound(err) {
		t.Errorf("binding should be deleted, err=%v", err)
	}

	// Finalizer should be removed.
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "del-sched"}, &delCR); err == nil {
		if controllerutil.ContainsFinalizer(&delCR, scheduleFinalizer) {
			t.Errorf("finalizer should be removed after deletion")
		}
	}
}

func TestScheduleReconciler_RoleBindingLongName(t *testing.T) {
	longName := ""
	for i := 0; i < 200; i++ {
		longName += "x"
	}
	cr := testSchedule(longName, "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: longName}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	bindingName := bindingNameFor(cr)
	if len(bindingName) > 63 {
		t.Errorf("binding name %q is %d chars, exceeds 63", bindingName, len(bindingName))
	}

	var binding v1alpha1.VarroaRoleBinding
	if err := r.client.Get(context.Background(), types.NamespacedName{Name: bindingName}, &binding); err != nil {
		t.Fatalf("get binding for long name: %v", err)
	}
	if binding.Annotations[bindingAnnName] != longName {
		t.Errorf("annotation name = %q, want %q", binding.Annotations[bindingAnnName], longName)
	}
}

func TestScheduleReconciler_RoleBindingOperatorNS(t *testing.T) {
	cr := testSchedule("op-sched", "operator-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "operator-ns", Name: "op-sched"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	bindingName := bindingNameFor(cr)
	var binding v1alpha1.VarroaRoleBinding
	if err := r.client.Get(context.Background(), types.NamespacedName{Name: bindingName}, &binding); err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if binding.Spec.RoleRef != "admin" {
		t.Errorf("roleRef = %q, want admin (operator namespace)", binding.Spec.RoleRef)
	}
	if binding.Spec.Scope != nil {
		t.Errorf("scope should be nil in operator namespace, got %+v", binding.Spec.Scope)
	}
}

// --- Task 3.5: CronJob command and spec ---

func TestScheduleReconciler_CronJobCommand(t *testing.T) {
	cr := testSchedule("cj-cmd", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	cr.Spec.WaitForCompletion = true
	cr.Spec.Template.Execution = &v1alpha1.BroodExecution{
		Order:         v1alpha1.BroodOrderName,
		MaxParallel:   int32Ptr(3),
		FailurePolicy: v1alpha1.BroodFailurePolicyFailFast,
	}
	cr.Spec.Template.TTLSecondsAfterFinished = int32Ptr(300)
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "cj-cmd"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var cj batchv1.CronJob
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "cj-cmd"}, &cj); err != nil {
		t.Fatalf("get cronjob: %v", err)
	}

	cmd := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command
	cmdStr := strings.Join(cmd, " ")
	if !strings.Contains(cmdStr, "--order=name") {
		t.Errorf("expected --order=name in command, got %v", cmd)
	}
	if !strings.Contains(cmdStr, "--max-parallel=3") {
		t.Errorf("expected --max-parallel=3 in command, got %v", cmd)
	}
	if !strings.Contains(cmdStr, "--failure-policy=FailFast") {
		t.Errorf("expected --failure-policy=FailFast in command, got %v", cmd)
	}
	if !strings.Contains(cmdStr, "--ttl=300") {
		t.Errorf("expected --ttl=300 in command, got %v", cmd)
	}
	if !strings.Contains(cmdStr, "--watch") {
		t.Errorf("expected --watch in command, got %v", cmd)
	}
	if !strings.Contains(cmdStr, "--names=ctrl-1") {
		t.Errorf("expected --names=ctrl-1 in command, got %v", cmd)
	}
}

func TestScheduleReconciler_CronJobCommandNilExecution(t *testing.T) {
	cr := testSchedule("cj-nil", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	cr.Spec.WaitForCompletion = false
	// execution and ttl left nil
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "cj-nil"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var cj batchv1.CronJob
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "cj-nil"}, &cj); err != nil {
		t.Fatalf("get cronjob: %v", err)
	}

	cmd := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command
	cmdStr := strings.Join(cmd, " ")
	if strings.Contains(cmdStr, "--order=") {
		t.Errorf("unexpected --order flag when execution is nil")
	}
	if strings.Contains(cmdStr, "--max-parallel=") {
		t.Errorf("unexpected --max-parallel flag when execution is nil")
	}
	if strings.Contains(cmdStr, "--failure-policy=") {
		t.Errorf("unexpected --failure-policy flag when execution is nil")
	}
	if !strings.Contains(cmdStr, "--ttl=86400") {
		t.Errorf("expected default --ttl=86400 when ttlSecondsAfterFinished is nil")
	}
	if strings.Contains(cmdStr, "--watch") {
		t.Errorf("unexpected --watch when waitForCompletion is false")
	}
}

func TestScheduleBuildCommand_TTLExplicitValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		ttl  *int32
		want string
	}{
		{name: "nil-default", want: "--ttl=86400"},
		{name: "zero-keeps-forever", ttl: int32Ptr(0), want: "--ttl=0"},
		{name: "explicit", ttl: int32Ptr(7200), want: "--ttl=7200"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cr := testSchedule("ttl-"+tt.name, "team-ns")
			cr.Spec.Template.TTLSecondsAfterFinished = tt.ttl
			if got := strings.Join(buildCommand(cr), " "); !strings.Contains(got, tt.want) {
				t.Fatalf("command = %q, want %s", got, tt.want)
			}
		})
	}
}

func TestScheduleReconciler_CronJobCommandSelectorJSON(t *testing.T) {
	cr := testSchedule("cj-sel", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	cr.Spec.Template.Targets = v1alpha1.BroodTargets{
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"team": "payments",
				"tier": "prod",
			},
		},
	}
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "cj-sel"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var cj batchv1.CronJob
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "cj-sel"}, &cj); err != nil {
		t.Fatalf("get cronjob: %v", err)
	}

	cmd := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command
	cmdStr := strings.Join(cmd, " ")
	if !strings.Contains(cmdStr, "--selector-json=") {
		t.Errorf("expected --selector-json= in command, got %v", cmd)
	}
	// Extract the --selector-json value.
	for _, part := range cmd {
		if strings.HasPrefix(part, "--selector-json=") {
			jsonPart := strings.TrimPrefix(part, "--selector-json=")
			var sel map[string]interface{}
			if err := json.Unmarshal([]byte(jsonPart), &sel); err != nil {
				t.Fatalf("unmarshal selector json: %v", err)
			}
			ml, ok := sel["matchLabels"].(map[string]interface{})
			if !ok {
				t.Fatalf("matchLabels not found in %v", sel)
			}
			if ml["team"] != "payments" || ml["tier"] != "prod" {
				t.Errorf("matchLabels = %+v, want {team:payments, tier:prod}", ml)
			}
		}
	}

	// Must NOT use --selector= (plain flag)
	if strings.Contains(cmdStr, " --selector=") {
		t.Errorf("unexpected --selector= flag (should use --selector-json)")
	}
}

func TestScheduleReconciler_CronJobBackoffAndRestart(t *testing.T) {
	cr := testSchedule("cj-safe", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "cj-safe"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var cj batchv1.CronJob
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "cj-safe"}, &cj); err != nil {
		t.Fatalf("get cronjob: %v", err)
	}

	js := cj.Spec.JobTemplate.Spec
	if js.BackoffLimit == nil || *js.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0", js.BackoffLimit)
	}
	if js.ActiveDeadlineSeconds == nil || *js.ActiveDeadlineSeconds != scheduleJobActiveDeadlineSeconds {
		t.Errorf("activeDeadlineSeconds = %v, want %d", js.ActiveDeadlineSeconds, scheduleJobActiveDeadlineSeconds)
	}
	if js.TTLSecondsAfterFinished == nil || *js.TTLSecondsAfterFinished != scheduleJobTTLSeconds {
		t.Errorf("ttlSecondsAfterFinished = %v, want %d", js.TTLSecondsAfterFinished, scheduleJobTTLSeconds)
	}
	ps := js.Template.Spec
	if ps.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %v, want Never", ps.RestartPolicy)
	}
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken = %v, want false", ps.AutomountServiceAccountToken)
	}
}

// The chart injects VARROA_IMAGE_PULL_SECRETS / VARROA_SCHEDULE_BFF_URL so the
// fired Job can pull the private varroactl image and reach the release-prefixed
// in-cluster BFF Service. Regression for the live-cluster ErrImagePull / wrong
// Service name found validating section 8.
func TestScheduleReconciler_CronJobImagePullSecretsAndBFFURL(t *testing.T) {
	t.Setenv("VARROA_IMAGE_PULL_SECRETS", "ghcr-cred, other-cred ")
	t.Setenv("VARROA_SCHEDULE_BFF_URL", "http://varroa-varroa-bff.varroa-system.svc:8080")

	cr := testSchedule("cj-pull", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "cj-pull"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var cj batchv1.CronJob
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "cj-pull"}, &cj); err != nil {
		t.Fatalf("get cronjob: %v", err)
	}
	if got := cj.Spec.JobTemplate.Spec.Template.Labels["app.kubernetes.io/component"]; got != "brood-schedule" {
		t.Errorf("pod component label = %q, want brood-schedule (BFF NetworkPolicy selects it)", got)
	}

	ps := cj.Spec.JobTemplate.Spec.Template.Spec

	if len(ps.Containers[0].Command) == 0 || ps.Containers[0].Command[0] != "/app/varroactl" {
		t.Errorf("command[0] = %v, want /app/varroactl (binary is not on $PATH)", ps.Containers[0].Command)
	}
	if cmd := strings.Join(ps.Containers[0].Command, " "); !strings.Contains(cmd, "--namespace=team-ns") {
		t.Errorf("command missing --namespace=team-ns (BFF would misroute to operator ns): %v", ps.Containers[0].Command)
	}
	if len(ps.ImagePullSecrets) != 2 ||
		ps.ImagePullSecrets[0].Name != "ghcr-cred" ||
		ps.ImagePullSecrets[1].Name != "other-cred" {
		t.Errorf("imagePullSecrets = %v, want [ghcr-cred other-cred]", ps.ImagePullSecrets)
	}

	var server string
	for _, e := range ps.Containers[0].Env {
		if e.Name == "VARROACTL_SERVER" {
			server = e.Value
		}
	}
	if server != "http://varroa-varroa-bff.varroa-system.svc:8080" {
		t.Errorf("VARROACTL_SERVER = %q, want injected in-cluster bff url", server)
	}
}

func TestScheduleReconciler_CronJobSuspendToggle(t *testing.T) {
	cr := testSchedule("cj-susp", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "cj-susp"}}

	// First reconcile — CronJob should be unsuspended.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var cj batchv1.CronJob
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "cj-susp"}, &cj); err != nil {
		t.Fatalf("get cronjob: %v", err)
	}
	if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
		t.Errorf("expected suspend=false initially")
	}

	// Set suspend on the schedule CR.
	if err := r.client.Get(context.Background(), req.NamespacedName, cr); err != nil {
		t.Fatalf("get cr: %v", err)
	}
	cr.Spec.Suspend = true
	if err := r.client.Update(context.Background(), cr); err != nil {
		t.Fatalf("update cr with suspend=true: %v", err)
	}

	// Reconcile — CronJob should become suspended.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "cj-susp"}, &cj); err != nil {
		t.Fatalf("get cronjob: %v", err)
	}
	if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
		t.Errorf("expected suspend=true after user suspension")
	}
}

// --- Task 3.6: Status copy ---

func TestScheduleReconciler_StatusCopy(t *testing.T) {
	cr := testSchedule("st-cp", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "st-cp"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	var sched v1alpha1.BroodSchedule
	if err := r.client.Get(context.Background(), req.NamespacedName, &sched); err != nil {
		t.Fatalf("get schedule: %v", err)
	}

	// CronJob should exist.
	var cj batchv1.CronJob
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "st-cp"}, &cj); err != nil {
		t.Fatalf("get cronjob: %v", err)
	}

	// Status Reason should be empty (no violation).
	if sched.Status.Reason != "" {
		t.Errorf("reason = %q, want empty", sched.Status.Reason)
	}

	// Test that status is copied regardless of waitForCompletion.
	// For the fake client we can't easily update CronJob status,
	// but we verify the CronJob was created and the schedule's status was persisted.
	cr2 := testSchedule("st-cp2", "team-ns")
	cr2.Finalizers = append(cr2.Finalizers, scheduleFinalizer)
	cr2.Spec.WaitForCompletion = false
	r2, _ := newTestScheduleReconciler(t, cr2)

	req2 := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "st-cp2"}}
	if _, err := r2.Reconcile(context.Background(), req2); err != nil {
		t.Fatalf("first reconcile st-cp2: %v", err)
	}

	if err := r2.client.Get(context.Background(), req2.NamespacedName, &sched); err != nil {
		t.Fatalf("get schedule2: %v", err)
	}
	if sched.Status.Reason != "" {
		t.Errorf("reason = %q, want empty", sched.Status.Reason)
	}

	var cj2 batchv1.CronJob
	if err := r2.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "st-cp2"}, &cj2); err != nil {
		t.Fatalf("get cronjob2: %v", err)
	}
	_ = cj2
}

// --- Task 3.9: Tenancy violation ---

func TestScheduleReconciler_TenancyViolationTeamNSWithNamespaces(t *testing.T) {
	cr := testSchedule("tv-ns", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	cr.Spec.Template.Targets.Namespaces = []string{"other-ns"} // not allowed in team namespace
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "tv-ns"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Should have TenancyViolation reason.
	var sched v1alpha1.BroodSchedule
	if err := r.client.Get(context.Background(), req.NamespacedName, &sched); err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if sched.Status.Reason != "TenancyViolation" {
		t.Errorf("reason = %q, want TenancyViolation", sched.Status.Reason)
	}

	// No CronJob should be created.
	var cj batchv1.CronJob
	err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "tv-ns"}, &cj)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no cronjob, got %v", cj)
	}
}

func TestScheduleReconciler_TenancyViolationMultipleClustersTeamNS(t *testing.T) {
	cr := testSchedule("tv-cl", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	cr.Spec.Template.Clusters = []string{"cluster-a", "cluster-b"} // >1 cluster in team ns
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "tv-cl"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var sched v1alpha1.BroodSchedule
	if err := r.client.Get(context.Background(), req.NamespacedName, &sched); err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if sched.Status.Reason != "TenancyViolation" {
		t.Errorf("reason = %q, want TenancyViolation", sched.Status.Reason)
	}
}

func TestScheduleReconciler_TenancyViolationExistingCronJob(t *testing.T) {
	cr := testSchedule("tv-ex", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "tv-ex"}}

	// First reconcile — creates CronJob (no violation yet).
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	// Now edit the schedule to add namespaces (tenancy violation).
	if err := r.client.Get(context.Background(), req.NamespacedName, cr); err != nil {
		t.Fatalf("get cr: %v", err)
	}
	cr.Spec.Template.Targets.Namespaces = []string{"other-ns"}
	if err := r.client.Update(context.Background(), cr); err != nil {
		t.Fatalf("update cr: %v", err)
	}

	// Reconcile — should detect violation, suspend the CronJob.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	var sched v1alpha1.BroodSchedule
	if err := r.client.Get(context.Background(), req.NamespacedName, &sched); err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if sched.Status.Reason != "TenancyViolation" {
		t.Errorf("reason = %q, want TenancyViolation", sched.Status.Reason)
	}

	// CronJob should now be suspended.
	var cj batchv1.CronJob
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "tv-ex"}, &cj); err != nil {
		t.Fatalf("get cronjob: %v", err)
	}
	if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
		t.Errorf("expected suspend=true after tenancy violation")
	}

	// Now fix the violation.
	if err := r.client.Get(context.Background(), req.NamespacedName, cr); err != nil {
		t.Fatalf("get cr: %v", err)
	}
	cr.Spec.Template.Targets.Namespaces = nil
	if err := r.client.Update(context.Background(), cr); err != nil {
		t.Fatalf("update cr: %v", err)
	}

	// Reconcile — should clear violation and unsuspend.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}

	if err := r.client.Get(context.Background(), req.NamespacedName, &sched); err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if sched.Status.Reason != "" {
		t.Errorf("reason = %q, want empty after fix", sched.Status.Reason)
	}

	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "tv-ex"}, &cj); err != nil {
		t.Fatalf("get cronjob: %v", err)
	}
	if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
		t.Errorf("expected suspend=false after tenancy fix")
	}
}

func TestScheduleReconciler_TenancyViolationUserSuspended(t *testing.T) {
	cr := testSchedule("tv-us", "team-ns")
	cr.Finalizers = append(cr.Finalizers, scheduleFinalizer)
	cr.Spec.Suspend = true                                     // user-suspended
	cr.Spec.Template.Targets.Namespaces = []string{"other-ns"} // also tenancy-violated
	r, _ := newTestScheduleReconciler(t, cr)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team-ns", Name: "tv-us"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var cj batchv1.CronJob
	if err := r.client.Get(context.Background(), types.NamespacedName{Namespace: "team-ns", Name: "tv-us"}, &cj); err == nil {
		// If CronJob exists, check it's suspended.
		if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
			t.Errorf("expected suspend=true for user-suspended + violated")
		}
	}
}

// --- Helpers ---

func int32Ptr(i int32) *int32 { return &i }
