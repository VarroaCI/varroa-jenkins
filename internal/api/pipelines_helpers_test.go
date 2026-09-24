package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// --- fakeRecordReader: a minimal, in-memory recordstore.Reader ---
//
// Embedding recordstore.Reader (nil) means an unconfigured method panics
// on use, which catches a test depending on a method this fake does not
// yet model.
type fakeRecordReader struct {
	recordstore.Reader

	schemaTooOld bool

	// jobSummaries is keyed by "<cluster>/<tenant>".
	jobSummaries map[string][]recordstore.JobSummary
	// runsByTenant is keyed by "<cluster>/<tenant>"; ListRuns filters and
	// pages this slice the way the real reader's SQL query does.
	runsByTenant map[string][]recordstore.Run
	// runs is keyed by run id, for GetRun/ListArtifacts/ListTests.
	runs      map[string]recordstore.Run
	stages    map[string][]recordstore.Stage
	artifacts map[string][]recordstore.Artifact
	tests     map[string][]recordstore.TestSuite
	intents   map[string]recordstore.Intent

	// windowStats is keyed by "<cluster>/<tenant>/<jobPath>", for
	// JobWindowStats.
	windowStats    map[string]recordstore.JobWindowStats
	windowStatsErr error

	// jobDependencies is keyed by "<cluster>/<tenant>/<jobPath>", for
	// JobDependencies. An unset key answers the zero value: two empty,
	// non-nil edge lists.
	jobDependencies map[string]recordstore.JobDependencies

	// jobHomes is keyed by "<cluster>/<tenant>/<jobPath>" and answers the
	// member that holds that job's home slot. An unset key answers
	// ok=false, which leaves the caller's own coordinate unchanged.
	jobHomes map[string]jobHomeAnswer
	// jobHomeErr makes every JobHome call fail, for the read-error case.
	jobHomeErr error

	// locksByTenant is keyed by "<cluster>/<tenant>", for
	// ListFleetLocks. It is seeded whole, so a test decides exactly
	// which rows one tenant's read answers.
	locksByTenant map[string][]recordstore.FleetLock
	// locksByCluster is keyed by cluster, for ListAllFleetLocks.
	locksByCluster map[string][]recordstore.FleetLock
	// locksErr makes both lock reads fail.
	locksErr error

	// testCases is keyed by run id, for ListTestCases. It answers the
	// seeded order unchanged, so a test seeds the reader's own order.
	testCases map[string][]recordstore.TestCase
	// testCasesErr makes every ListTestCases call fail.
	testCasesErr error
	// caseHistory is keyed by caseHistoryKey, for ListTestCaseHistory.
	// Each slice is seeded newest first, the reader's own order.
	caseHistory map[string][]recordstore.TestCaseOutcome
	// historyQueries records every ListTestCaseHistory query in call
	// order.
	historyQueries []recordstore.TestCaseHistoryQuery
}

// caseHistoryKey is caseHistory's key for one tenant, job, and case.
func caseHistoryKey(cluster, tenant, jobPath, className, name string) string {
	return cluster + "/" + tenant + "/" + jobPath + "/" + className + "/" + name
}

// ListTestCases answers the seeded case rows of one run.
func (f *fakeRecordReader) ListTestCases(_ context.Context, runID string) ([]recordstore.TestCase, error) {
	if f.schemaTooOld {
		return nil, recordstore.ErrSchemaTooOld
	}
	if f.testCasesErr != nil {
		return nil, f.testCasesErr
	}
	return f.testCases[runID], nil
}

// ListTestCaseHistory answers the seeded outcomes of one case, cut to the
// query's limit.
func (f *fakeRecordReader) ListTestCaseHistory(_ context.Context, q recordstore.TestCaseHistoryQuery) ([]recordstore.TestCaseOutcome, error) {
	if f.schemaTooOld {
		return nil, recordstore.ErrSchemaTooOld
	}
	f.historyQueries = append(f.historyQueries, q)
	rows := f.caseHistory[caseHistoryKey(q.Cluster, q.Tenant, q.JobPath, q.ClassName, q.Name)]
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return rows, nil
}

// jobHomeAnswer is one fake JobHome result.
type jobHomeAnswer struct {
	namespace string
	cell      string
}

// JobHome answers the seeded home member for a tenant and a job path. An
// unset key answers Placed false, which is a tenant with no placement row.
func (f *fakeRecordReader) JobHome(_ context.Context, cluster, tenant, jobPath string) (recordstore.JobHome, error) {
	if f.jobHomeErr != nil {
		return recordstore.JobHome{}, f.jobHomeErr
	}
	home, ok := f.jobHomes[cluster+"/"+tenant+"/"+jobPath]
	if !ok {
		return recordstore.JobHome{}, nil
	}
	return recordstore.JobHome{Placed: true, Namespace: home.namespace, Cell: home.cell}, nil
}

// ListFleetLocks answers the seeded rows for one tenant.
func (f *fakeRecordReader) ListFleetLocks(_ context.Context, cluster, tenant string) ([]recordstore.FleetLock, error) {
	if f.locksErr != nil {
		return nil, f.locksErr
	}
	return f.locksByTenant[f.tenantKey(cluster, tenant)], nil
}

// ListAllFleetLocks answers the seeded rows of one cluster.
func (f *fakeRecordReader) ListAllFleetLocks(_ context.Context, cluster string) ([]recordstore.FleetLock, error) {
	if f.locksErr != nil {
		return nil, f.locksErr
	}
	return f.locksByCluster[cluster], nil
}

func newFakeRecordReader() *fakeRecordReader {
	return &fakeRecordReader{
		jobSummaries:    map[string][]recordstore.JobSummary{},
		runsByTenant:    map[string][]recordstore.Run{},
		runs:            map[string]recordstore.Run{},
		stages:          map[string][]recordstore.Stage{},
		artifacts:       map[string][]recordstore.Artifact{},
		tests:           map[string][]recordstore.TestSuite{},
		jobHomes:        map[string]jobHomeAnswer{},
		intents:         map[string]recordstore.Intent{},
		windowStats:     map[string]recordstore.JobWindowStats{},
		jobDependencies: map[string]recordstore.JobDependencies{},
		locksByTenant:   map[string][]recordstore.FleetLock{},
		locksByCluster:  map[string][]recordstore.FleetLock{},
		testCases:       map[string][]recordstore.TestCase{},
		caseHistory:     map[string][]recordstore.TestCaseOutcome{},
	}
}

func (f *fakeRecordReader) tenantKey(cluster, tenant string) string { return cluster + "/" + tenant }

// addRun seeds one run under both runsByTenant (for ListRuns) and runs
// (for GetRun and its sub-resources).
func (f *fakeRecordReader) addRun(run recordstore.Run) {
	key := f.tenantKey(run.Cluster, run.Tenant)
	f.runsByTenant[key] = append(f.runsByTenant[key], run)
	f.runs[run.RunID] = run
}

func (f *fakeRecordReader) ListJobSummaries(_ context.Context, cluster, tenant string, _ time.Time) ([]recordstore.JobSummary, error) {
	if f.schemaTooOld {
		return nil, recordstore.ErrSchemaTooOld
	}
	return f.jobSummaries[f.tenantKey(cluster, tenant)], nil
}

func (f *fakeRecordReader) GetJobSummary(_ context.Context, cluster, tenant, jobPath string) (recordstore.JobSummary, error) {
	if f.schemaTooOld {
		return recordstore.JobSummary{}, recordstore.ErrSchemaTooOld
	}
	for _, js := range f.jobSummaries[f.tenantKey(cluster, tenant)] {
		if js.JobPath == jobPath {
			return js, nil
		}
	}
	return recordstore.JobSummary{JobPath: jobPath}, nil
}

// fakeCursorDecode/fakeCursorEncode use the exact format
// handlers_pipelines.go's runResumeCursor produces, so a cursor this
// fake issues and a cursor the handler builds from a partially consumed
// batch both decode here the same way — matching the real reader's
// encodeRunCursor/decodeRunCursor contract.
func fakeCursorDecode(s string) (time.Time, string, bool) {
	if s == "" {
		return time.Time{}, "", false
	}
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", false
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", false
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", false
	}
	return ts, parts[1], true
}

func fakeCursorEncode(ts time.Time, runID string) string {
	raw := ts.UTC().Format(time.RFC3339Nano) + "|" + runID
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

func (f *fakeRecordReader) ListRuns(_ context.Context, q recordstore.RunQuery) (recordstore.RunPage, error) {
	if f.schemaTooOld {
		return recordstore.RunPage{}, recordstore.ErrSchemaTooOld
	}
	all := append([]recordstore.Run(nil), f.runsByTenant[f.tenantKey(q.Cluster, q.Tenant)]...)
	var filtered []recordstore.Run
	for _, r := range all {
		if q.JobPath != "" && r.JobPath != q.JobPath {
			continue
		}
		if q.Branch != "" && (r.Branch == nil || *r.Branch != q.Branch) {
			continue
		}
		if q.Result != "" && (r.Result == nil || *r.Result != q.Result) {
			continue
		}
		if q.Actor != "" && (r.Actor == nil || *r.Actor != q.Actor) {
			continue
		}
		if q.Status != "" && r.Status != q.Status {
			continue
		}
		filtered = append(filtered, r)
	}
	sort.Slice(filtered, func(i, j int) bool { return runAfter(filtered[i], filtered[j]) })

	start := 0
	if q.Cursor != "" {
		if ts, id, ok := fakeCursorDecode(q.Cursor); ok {
			start = len(filtered)
			for i, r := range filtered {
				if r.CreatedAt.Before(ts) || (r.CreatedAt.Equal(ts) && r.RunID < id) {
					start = i
					break
				}
			}
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	var page recordstore.RunPage
	end := start + limit
	if end >= len(filtered) {
		if start < len(filtered) {
			page.Runs = filtered[start:]
		}
	} else {
		page.Runs = filtered[start:end]
		last := filtered[end-1]
		page.NextCursor = fakeCursorEncode(last.CreatedAt, last.RunID)
	}
	return page, nil
}

func (f *fakeRecordReader) GetRun(_ context.Context, runID string) (recordstore.Run, []recordstore.Stage, error) {
	if f.schemaTooOld {
		return recordstore.Run{}, nil, recordstore.ErrSchemaTooOld
	}
	run, ok := f.runs[runID]
	if !ok {
		return recordstore.Run{}, nil, recordstore.ErrRunNotFound
	}
	return run, f.stages[runID], nil
}

func (f *fakeRecordReader) ListArtifacts(_ context.Context, runID string) ([]recordstore.Artifact, error) {
	if f.schemaTooOld {
		return nil, recordstore.ErrSchemaTooOld
	}
	return f.artifacts[runID], nil
}

func (f *fakeRecordReader) ListTests(_ context.Context, runID string) ([]recordstore.TestSuite, error) {
	if f.schemaTooOld {
		return nil, recordstore.ErrSchemaTooOld
	}
	return f.tests[runID], nil
}

// CurrentStages answers the top-level stage in progress for each run, read
// from the stages this fake holds. It applies the reader's own predicate:
// only a row that carries node_type "stage" and no parent competes, and
// the IN_PROGRESS stage with the latest startedAt then wins, with the
// latest createdAt breaking a tie. A run that holds no such stage carries
// no entry.
func (f *fakeRecordReader) CurrentStages(_ context.Context, runIDs []string) (map[string]string, error) {
	stages := map[string]string{}
	for _, runID := range runIDs {
		best := -1
		for i, s := range f.stages[runID] {
			if s.Status != "IN_PROGRESS" || s.NodeType != "stage" || s.ParentID != nil {
				continue
			}
			if best < 0 || laterStage(s, f.stages[runID][best]) {
				best = i
			}
		}
		if best >= 0 {
			stages[runID] = f.stages[runID][best].Name
		}
	}
	return stages, nil
}

// laterStage reports whether a is the newer of two stages in progress: a
// stage that has started beats one that has no startedAt yet, and the
// created time breaks a tie.
func laterStage(a, b recordstore.Stage) bool {
	if (a.StartedAt != nil) != (b.StartedAt != nil) {
		return a.StartedAt != nil
	}
	if a.StartedAt != nil && !a.StartedAt.Equal(*b.StartedAt) {
		return a.StartedAt.After(*b.StartedAt)
	}
	return a.CreatedAt.After(b.CreatedAt)
}

// setWindowStats seeds the JobWindowStats result for one cluster,
// tenant, and job path, for a test to assert the "Last 30 days" card.
func (f *fakeRecordReader) setWindowStats(cluster, tenant, jobPath string, stats recordstore.JobWindowStats) {
	f.windowStats[f.tenantKey(cluster, tenant)+"/"+jobPath] = stats
}

func (f *fakeRecordReader) JobWindowStats(_ context.Context, cluster, tenant, jobPath string, _ time.Time) (recordstore.JobWindowStats, error) {
	if f.windowStatsErr != nil {
		return recordstore.JobWindowStats{}, f.windowStatsErr
	}
	return f.windowStats[f.tenantKey(cluster, tenant)+"/"+jobPath], nil
}

// setJobDependencies seeds the JobDependencies result for one cluster,
// tenant, and job path, for a test to assert the Triggers card's
// dependency edges.
func (f *fakeRecordReader) setJobDependencies(cluster, tenant, jobPath string, deps recordstore.JobDependencies) {
	f.jobDependencies[f.tenantKey(cluster, tenant)+"/"+jobPath] = deps
}

func (f *fakeRecordReader) JobDependencies(_ context.Context, cluster, tenant, jobPath string) (recordstore.JobDependencies, error) {
	if f.schemaTooOld {
		return recordstore.JobDependencies{}, recordstore.ErrSchemaTooOld
	}
	deps, ok := f.jobDependencies[f.tenantKey(cluster, tenant)+"/"+jobPath]
	if !ok {
		return recordstore.JobDependencies{Upstream: []recordstore.DependencyEdge{}, Downstream: []recordstore.DependencyEdge{}}, nil
	}
	return deps, nil
}

func (f *fakeRecordReader) GetIntentByKey(_ context.Context, key string) (recordstore.Intent, bool, error) {
	if f.schemaTooOld {
		return recordstore.Intent{}, false, recordstore.ErrSchemaTooOld
	}
	it, ok := f.intents[key]
	return it, ok, nil
}

// --- fakeBlobStore: a minimal, in-memory blob.Store ---

type fakeBlobStore struct {
	blob.Store
	objects map[string][]byte
	err     error
}

func newFakeBlobStore() *fakeBlobStore { return &fakeBlobStore{objects: map[string][]byte{}} }

func (f *fakeBlobStore) NewObjectReader(_ context.Context, key string, offset, length int64) (io.ReadCloser, int64, error) {
	if f.err != nil {
		return nil, 0, f.err
	}
	data, ok := f.objects[key]
	if !ok {
		return nil, 0, blob.ErrObjectNotFound
	}
	total := int64(len(data))
	if offset > total {
		offset = total
	}
	end := total
	if length >= 0 && offset+length < end {
		end = offset + length
	}
	return io.NopCloser(bytes.NewReader(data[offset:end])), total, nil
}

// --- shared authorization fixture ---

// pipelineGrantAuthorizer grants verbs on "pipelines" for every tenant
// string listed, with no job-path glob restriction. A tenant with no "/"
// parses as a Team tenant and is granted through a Tenants-scoped
// binding; a tenant of the form "<namespace>/<controller>" parses as a
// controller tenant and is granted through a Namespaces-scoped binding,
// because rbac.Resolver.effectiveControllerTenantGrants reads only
// Namespaces and a nil scope for a controller tenant — never Tenants.
func pipelineGrantAuthorizer(verbs []string, tenants ...string) *Authorizer {
	role := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "pl"},
		Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"pipelines"}, Verbs: verbs}},
		},
	}
	var bindings []*v1alpha1.VarroaRoleBinding
	var teamScope v1alpha1.VarroaRoleBindingScope
	var namespaces []string
	for _, t := range tenants {
		if idx := strings.Index(t, "/"); idx >= 0 {
			namespaces = append(namespaces, t[:idx])
			continue
		}
		teamScope.Tenants = append(teamScope.Tenants, v1alpha1.TenantScope{Name: t})
	}
	if len(teamScope.Tenants) > 0 {
		bindings = append(bindings, &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "team-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
				RoleRef:  "pl",
				Scope:    &teamScope,
			},
		})
	}
	if len(namespaces) > 0 {
		bindings = append(bindings, &v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "u"}},
				RoleRef:  "pl",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: namespaces},
			},
		})
	}
	return NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{role}, bindings), false)
}

// pipelinesTestServer assembles a *Server wired for the pipelines read
// paths: a fakeResourceClient-backed crdstore.Backend and Brood, a
// TenantResolver built from them, and the caller's record store, blob
// store, and authorizer.
func pipelinesTestServer(client *fakeResourceClient, authorizer *Authorizer, reader recordstore.Reader, blobStore blob.Store) *Server {
	store := storeFromFake(client)
	brood := newFakeBrood(client)
	deps := &Dependencies{
		Store:             store,
		Client:            client,
		Brood:             brood,
		Authorizer:        authorizer,
		RecordStore:       reader,
		BlobStore:         blobStore,
		TenantResolver:    NewTeamTenantResolver(store, brood, authorizer),
		Logger:            slog.Default(),
		OperatorNamespace: "test-ns",
	}
	return NewServer(deps)
}
