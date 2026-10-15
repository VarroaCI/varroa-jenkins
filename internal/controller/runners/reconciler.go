// Package runners implements the leader-elected Runner reconciler (design
// D2) and the shared node renderer the Controller reconciler's
// desired-state assembly calls through BuildContribution. A Runner binds
// one external machine — reached over ssh or connecting inbound through the
// agent door — to one pipelines tenant's cell.
package runners

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// probeTokenTTL is the operator Jenkins token lifetime for one liveness
// probe (design D10).
const probeTokenTTL = 60 * time.Second

// tokenBytes is the mint size for an inbound runner's door token: 16 bytes
// of crypto/rand rendered as hex, matching Team.status.pipelines.webhookToken.
const tokenBytes = 16

// OperatorTokenSigner mints the operator's short-lived direct-to-Jenkins
// identity token. Satisfied by *internal/mite.MiteTokenSigner in
// production; a test supplies a stub.
type OperatorTokenSigner interface {
	GenerateOperatorJenkinsToken(name, ns string, ttl time.Duration) (string, error)
}

// CellKey names one cell Controller, including the cluster it runs on: a
// cell name is unique only within one cluster.
type CellKey struct {
	Cluster   string
	Namespace string
	Name      string
}

// TickResult reports what one Tick call did, for the caller to enqueue.
type TickResult struct {
	// TouchedCells lists every cell the caller must reconcile: every cell
	// that hosts a runner this tick, plus every cell a runner left.
	TouchedCells []CellKey
}

// Reconciler is the leader-elected Runner ticker (design D2). It writes
// Runner.status and nothing else, and hands the Controller reconciler no
// in-memory state: BuildContribution reads the CRDs itself.
type Reconciler struct {
	// Backend is the crdstore seam every read and status patch goes
	// through.
	Backend crdstore.Backend
	// Secrets resolves an ssh Runner's SSH credential for tick step 7
	// (design D2, D5). The operator's own ResourceClient satisfies it with
	// no adapter.
	Secrets SecretReader
	// HTTPClient serves the liveness probe of design D10. One client with
	// keep-alive serves every runner of every cell.
	HTTPClient *http.Client
	// TokenSigner mints the 60-second operator Jenkins token the liveness
	// probe carries.
	TokenSigner OperatorTokenSigner
	Logger      *slog.Logger
	// ServiceURL builds a cell's in-cluster Jenkins Service base URL. Nil
	// means CellServiceURL, the production formula; a test substitutes an
	// httptest server's URL.
	ServiceURL func(cr *v1alpha1.Controller) string

	// mu guards lastCellForRunner, the leader-local enqueue-hint map
	// (design D2 step 10): a list of the live Runner objects cannot name
	// the cell a deleted runner left, so the previous tick's map is kept
	// here. It is an enqueue hint only — it never decides what a cell
	// renders, and an empty map costs one reconcile interval of delay,
	// never a wrong node list.
	mu                sync.Mutex
	lastCellForRunner map[string]CellKey
}

func (r *Reconciler) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *Reconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return http.DefaultClient
}

func (r *Reconciler) serviceURL(cr *v1alpha1.Controller) string {
	if r.ServiceURL != nil {
		return r.ServiceURL(cr)
	}
	return CellServiceURL(cr)
}

// Tick runs one pass of design D2's ten-step algorithm. It lists every Team
// and every Runner; a list failure is an early transport fault and the tick
// returns with no per-runner processing and no status patch, so
// observedGeneration never advances on it. Otherwise it evaluates and
// patches every Runner once, then reports which cells the caller must
// enqueue.
func (r *Reconciler) Tick(ctx context.Context) TickResult {
	log := r.logger()

	teams, err := crdstore.List[v1alpha1.Team](ctx, r.Backend, "", "")
	if err != nil {
		log.Warn("runner tick: list teams failed", "error", err)
		return TickResult{}
	}
	runnerList, err := crdstore.List[v1alpha1.Runner](ctx, r.Backend, "", "")
	if err != nil {
		log.Warn("runner tick: list runners failed", "error", err)
		return TickResult{}
	}

	teamsByName := make(map[string]*v1alpha1.Team, len(teams))
	for _, t := range teams {
		teamsByName[t.Name] = t
	}

	now := time.Now()
	newMap := make(map[string]CellKey, len(runnerList))
	cellSet := make(map[CellKey]bool)

	for _, runner := range runnerList {
		team := teamsByName[runner.Spec.Team]
		eval := r.evaluateWithCredential(ctx, runner, team)
		r.applyTick(ctx, runner, eval, now, log)

		if eval.Cell != "" && eval.CellNamespace != "" {
			key := CellKey{Cluster: eval.Cluster, Namespace: eval.CellNamespace, Name: eval.Cell}
			newMap[runner.Name] = key
			cellSet[key] = true
		}
	}

	r.mu.Lock()
	oldMap := r.lastCellForRunner
	r.lastCellForRunner = newMap
	r.mu.Unlock()

	for name, oldKey := range oldMap {
		if newKey, ok := newMap[name]; !ok || newKey != oldKey {
			cellSet[oldKey] = true
		}
	}

	cells := make([]CellKey, 0, len(cellSet))
	for k := range cellSet {
		cells = append(cells, k)
	}
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Cluster != cells[j].Cluster {
			return cells[i].Cluster < cells[j].Cluster
		}
		if cells[i].Namespace != cells[j].Namespace {
			return cells[i].Namespace < cells[j].Namespace
		}
		return cells[i].Name < cells[j].Name
	})

	return TickResult{TouchedCells: cells}
}

// evaluateWithCredential applies tick step 7 on top of evaluateRunner's
// steps 3 to 6: for an ssh runner steps 3 to 6 left delivered, it reads the
// credential Secret and turns Delivered false with reason
// CredentialMissing or CredentialMalformed on a fault. BuildContribution
// applies the same read for the same runner on its own separate call, so
// the reported status and the rendered node set can never disagree.
func (r *Reconciler) evaluateWithCredential(ctx context.Context, runner *v1alpha1.Runner, team *v1alpha1.Team) evaluation {
	eval := evaluateRunner(runner, team)
	if !eval.Delivered || runner.Spec.Launcher != launcherSSH {
		return eval
	}
	agentNamespace := ""
	if team != nil && team.Spec.Pipelines != nil {
		agentNamespace = team.Spec.Pipelines.AgentNamespace
	}
	if _, fault := readCredential(ctx, r.Secrets, runner, agentNamespace); fault != nil {
		eval.Delivered = false
		eval.Reason = fault.Reason
		eval.Message = fault.Message
	}
	return eval
}

// applyTick computes runner's new status from eval, mints a door token when
// due (task 3.3), probes liveness for a delivered runner (task 3.4), and
// patches the status once, only when a value changed.
func (r *Reconciler) applyTick(ctx context.Context, runner *v1alpha1.Runner, eval evaluation, now time.Time, log *slog.Logger) {
	newStatus := v1alpha1.RunnerStatus{
		NodeName:           eval.NodeName,
		Cell:               eval.Cell,
		CellNamespace:      eval.CellNamespace,
		Token:              runner.Status.Token,
		Connected:          runner.Status.Connected,
		LastSeen:           runner.Status.LastSeen,
		ObservedGeneration: runner.Generation,
		Conditions:         runner.Status.Conditions,
	}

	// Task 3.3: mint once for an inbound runner, never for ssh, never
	// replaced.
	if runner.Spec.Launcher == launcherInbound && newStatus.Token == "" {
		tok, err := mintToken()
		if err != nil {
			log.Warn("runner tick: mint token failed", "runner", runner.Name, "error", err)
		} else {
			newStatus.Token = tok
		}
	}

	reason, message := eval.Reason, eval.Message
	if !eval.Delivered {
		// An undelivered runner has no node on any cell, so a connected
		// value from an earlier tick never carries forward.
		newStatus.Connected = false
	} else {
		online, err := r.probe(ctx, eval.Cell, eval.CellNamespace, eval.NodeName)
		if err != nil || !online {
			newStatus.Connected = false
			reason = ReasonOffline
			if err != nil {
				message = err.Error()
			} else {
				message = "the cell reports this node offline"
			}
		} else {
			newStatus.Connected = true
			t := metav1.NewTime(now)
			newStatus.LastSeen = &t
			reason = ReasonConnected
			message = "the cell reports this node online"
		}
	}

	readyStatus := metav1.ConditionFalse
	if eval.Delivered && reason == ReasonConnected {
		readyStatus = metav1.ConditionTrue
	}
	newStatus.Conditions = upsertReadyCondition(runner.Status.Conditions, readyStatus, reason, message, metav1.NewTime(now))

	if reflect.DeepEqual(runner.Status, newStatus) {
		return
	}
	if err := crdstore.PatchStatus[v1alpha1.Runner](ctx, r.Backend, runner.Name, "", &newStatus); err != nil {
		log.Warn("runner tick: patch status failed", "runner", runner.Name, "error", err)
	}
}

// upsertReadyCondition upserts the Ready condition, preserving
// LastTransitionTime when neither Status, Reason, nor Message changed —
// mirroring the setCondition pattern every reconciler in this codebase
// uses.
func upsertReadyCondition(existing []v1alpha1.RunnerCondition, status metav1.ConditionStatus, reason, message string, now metav1.Time) []v1alpha1.RunnerCondition {
	cond := v1alpha1.RunnerCondition{
		Type:               ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	}
	out := make([]v1alpha1.RunnerCondition, 0, len(existing)+1)
	found := false
	for _, e := range existing {
		if e.Type == ConditionTypeReady {
			if e.Status == cond.Status && e.Reason == cond.Reason && e.Message == cond.Message {
				cond.LastTransitionTime = e.LastTransitionTime
			}
			out = append(out, cond)
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		out = append(out, cond)
	}
	return out
}

// mintToken returns 16 bytes of crypto/rand as hex, matching
// Team.status.pipelines.webhookToken's mint rule.
func mintToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint runner token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// probe implements design D10's liveness read: GET the cell's per-node API
// with a 60-second operator Jenkins token and report the offline flag's
// negation. A Controller lookup failure, a token mint failure, a transport
// error, or a non-200 answer all report an error; the caller treats every
// one as Offline and leaves lastSeen unchanged.
func (r *Reconciler) probe(ctx context.Context, cellName, cellNamespace, nodeName string) (bool, error) {
	cell, err := crdstore.Get[v1alpha1.Controller](ctx, r.Backend, cellName, cellNamespace)
	if err != nil {
		return false, fmt.Errorf("get cell %s/%s: %w", cellNamespace, cellName, err)
	}
	token, err := r.TokenSigner.GenerateOperatorJenkinsToken(cellName, cellNamespace, probeTokenTTL)
	if err != nil {
		return false, fmt.Errorf("mint operator jenkins token: %w", err)
	}

	url := r.serviceURL(cell) + "/computer/" + nodeName + "/api/json?tree=offline"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("build probe request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("probe %s: %w", nodeName, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("probe %s: status %d", nodeName, resp.StatusCode)
	}

	var body struct {
		Offline bool `json:"offline"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, fmt.Errorf("decode probe response for %s: %w", nodeName, err)
	}
	return !body.Offline, nil
}

// CellServiceURL builds the cell's in-cluster Jenkins Service URL
// (<name>-<uid[:8]>-svc.<namespace>.svc.cluster.local:8080). It must stay
// byte-identical with internal/api's BuildJenkinsServiceURL; this package
// cannot import internal/api without an import cycle back through
// internal/controller. It is exported so a caller that imports both
// packages can pin the two formulas to one expected string.
func CellServiceURL(cr *v1alpha1.Controller) string {
	uid := string(cr.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	prefix := cr.Name + "-" + uid
	return fmt.Sprintf("http://%s-svc.%s.svc.cluster.local:8080", prefix, cr.Namespace)
}
