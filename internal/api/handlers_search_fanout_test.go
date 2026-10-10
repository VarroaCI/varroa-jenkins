package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// searchFanoutBrood reports two clusters, core and member-b. ListAll
// returns the rows and statuses it was given.
type searchFanoutBrood struct {
	Brood
	controllers []ClusterController
	statuses    []ClusterFanoutStatus
	clustersErr error
}

func (b *searchFanoutBrood) LocalCluster() string { return "core" }

func (b *searchFanoutBrood) Clusters(context.Context) ([]ClusterInfo, error) {
	if b.clustersErr != nil {
		return nil, b.clustersErr
	}
	return []ClusterInfo{
		{ClusterInfo: bus.ClusterInfo{Name: "core"}, Core: true, Healthy: true},
		{ClusterInfo: bus.ClusterInfo{Name: "member-b"}, Healthy: true},
	}, nil
}

func (b *searchFanoutBrood) ListAll(context.Context, string, string) ([]ClusterController, []ClusterFanoutStatus, error) {
	return b.controllers, b.statuses, nil
}

// searchFanoutConfigBrood answers ListCatalogItems per cluster. A cluster
// named in errs fails with that error. Every other cluster returns one
// catalog item whose name contains "team".
type searchFanoutConfigBrood struct {
	ConfigBrood
	errs map[string]error
}

func (c *searchFanoutConfigBrood) ListCatalogItems(_ context.Context, cluster, _ string, _ CatalogItemFilter) ([]json.RawMessage, string, error) {
	if err := c.errs[cluster]; err != nil {
		return nil, "", err
	}
	raw, err := json.Marshal(bus.CatalogItemSummary{Name: "team-" + cluster, Namespace: "ns-a", Type: "pipeline"})
	if err != nil {
		return nil, "", err
	}
	return []json.RawMessage{raw}, "", nil
}

// searchFanoutAuthorizer grants controllers:read and catalogitems:read
// cluster-wide to user "fanout-user".
func searchFanoutAuthorizer() *Authorizer {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "fanout-viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{
				{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				{Resources: []string{"catalogitems"}, Verbs: []string{"read"}},
			},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "fanout-binding"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "fanout-user"}},
			RoleRef:  "fanout-viewer",
		}},
	}
	return NewAuthorizer(testResolver(roles, bindings), false)
}

var fanoutClaims = &auth.Claims{Subject: "fanout-user"}

func searchFanoutServer(brood Brood, config ConfigBrood) *Server {
	return NewServer(&Dependencies{
		Logger:      slog.Default(),
		Authorizer:  searchFanoutAuthorizer(),
		Brood:       brood,
		ConfigBrood: config,
	})
}

// coreTeamController is the one controller row that every fan-out test
// returns from the core cluster.
var coreTeamController = ClusterController{Cluster: "core", CR: &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "team-core", Namespace: "ns-a"}}}

func findFanoutRows(rows []ClusterFanoutStatus, name string) []ClusterFanoutStatus {
	var out []ClusterFanoutStatus
	for _, r := range rows {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

func TestSearch_ReportsFailedMemberCluster(t *testing.T) {
	brood := &searchFanoutBrood{
		controllers: []ClusterController{coreTeamController},
		statuses: []ClusterFanoutStatus{
			{Name: "core", OK: true},
			{Name: "member-b", OK: false, Error: "cluster unreachable"},
		},
	}
	server := searchFanoutServer(brood, &searchFanoutConfigBrood{})

	resp, err := server.Search(context.Background(), fanoutClaims, "team", "")
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	rows := findFanoutRows(resp.Clusters, "member-b")
	if len(rows) != 1 || rows[0].OK || rows[0].Error != "cluster unreachable" {
		t.Fatalf("member-b rows = %+v, want one ok false row with the brood error", resp.Clusters)
	}
	if len(findFanoutRows(resp.Clusters, "core")) != 1 {
		t.Fatalf("clusters = %+v, want a core row", resp.Clusters)
	}
	if len(resp.Items) == 0 {
		t.Fatalf("items empty, want the core controller match")
	}
}

func TestSearch_CatalogFailureMarksClusterIncomplete(t *testing.T) {
	brood := &searchFanoutBrood{
		controllers: []ClusterController{coreTeamController},
		statuses: []ClusterFanoutStatus{
			{Name: "core", OK: true},
			{Name: "member-b", OK: true},
		},
	}
	config := &searchFanoutConfigBrood{errs: map[string]error{"member-b": errors.New("catalog down")}}
	server := searchFanoutServer(brood, config)

	resp, err := server.Search(context.Background(), fanoutClaims, "team", "")
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	rows := findFanoutRows(resp.Clusters, "member-b")
	if len(rows) != 1 || rows[0].OK || !strings.HasPrefix(rows[0].Error, "catalog items: ") {
		t.Fatalf("member-b rows = %+v, want one ok false row prefixed catalog items:", resp.Clusters)
	}
	if !strings.Contains(rows[0].Error, "catalog down") {
		t.Fatalf("member-b error = %q, want the catalog error text", rows[0].Error)
	}
}

func TestSearch_CatalogFailureAppendsToFailedRow(t *testing.T) {
	brood := &searchFanoutBrood{
		controllers: []ClusterController{coreTeamController},
		statuses: []ClusterFanoutStatus{
			{Name: "core", OK: true},
			{Name: "member-b", OK: false, Error: "cluster unreachable"},
		},
	}
	config := &searchFanoutConfigBrood{errs: map[string]error{"member-b": errors.New("catalog down")}}
	server := searchFanoutServer(brood, config)

	resp, err := server.Search(context.Background(), fanoutClaims, "team", "")
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	rows := findFanoutRows(resp.Clusters, "member-b")
	if len(rows) != 1 {
		t.Fatalf("member-b rows = %+v, want exactly one row", resp.Clusters)
	}
	if rows[0].OK || !strings.Contains(rows[0].Error, "cluster unreachable") || !strings.Contains(rows[0].Error, "catalog items: catalog down") {
		t.Fatalf("member-b row = %+v, want both error texts", rows[0])
	}
}

func TestSearch_EmptyQueryReturnsEmptyClusters(t *testing.T) {
	server := searchFanoutServer(&searchFanoutBrood{}, &searchFanoutConfigBrood{})

	resp, err := server.Search(context.Background(), fanoutClaims, "   ", "")
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	if resp.Items == nil || len(resp.Items) != 0 {
		t.Fatalf("items = %#v, want non-nil empty", resp.Items)
	}
	if resp.Clusters == nil || len(resp.Clusters) != 0 {
		t.Fatalf("clusters = %#v, want non-nil empty", resp.Clusters)
	}
}

func TestSearch_ClusterListFailureMarksEveryRowIncomplete(t *testing.T) {
	brood := &searchFanoutBrood{
		controllers: []ClusterController{coreTeamController},
		statuses:    []ClusterFanoutStatus{{Name: "core", OK: true}},
		clustersErr: errors.New("kv unreachable"),
	}
	server := searchFanoutServer(brood, &searchFanoutConfigBrood{})

	resp, err := server.Search(context.Background(), fanoutClaims, "team", "")
	if err != nil {
		t.Fatalf("Search error = %v", err)
	}
	rows := findFanoutRows(resp.Clusters, "core")
	if len(rows) != 1 || rows[0].OK || !strings.Contains(rows[0].Error, "kv unreachable") {
		t.Fatalf("core row = %+v, want ok false with the cluster list error", resp.Clusters)
	}
}

func TestHandleSearch_EnvelopeCarriesClusters(t *testing.T) {
	brood := &searchFanoutBrood{
		controllers: []ClusterController{coreTeamController},
		statuses: []ClusterFanoutStatus{
			{Name: "core", OK: true},
			{Name: "member-b", OK: false, Error: "cluster unreachable"},
		},
	}
	server := searchFanoutServer(brood, &searchFanoutConfigBrood{})

	request := httptest.NewRequest("GET", "/api/v1/search?q=team", nil)
	request = request.WithContext(auth.ContextWithClaims(request.Context(), fanoutClaims))
	response := httptest.NewRecorder()
	server.handleSearch(response, request)

	if response.Code != 200 {
		t.Fatalf("status = %d body %s, want 200", response.Code, response.Body.String())
	}
	var body struct {
		Items    []SearchResult        `json:"items"`
		Clusters []ClusterFanoutStatus `json:"clusters"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) == 0 {
		t.Fatalf("items empty in %s", response.Body.String())
	}
	rows := findFanoutRows(body.Clusters, "member-b")
	if len(rows) != 1 || rows[0].OK || rows[0].Error != "cluster unreachable" {
		t.Fatalf("clusters = %s, want the member-b failure", response.Body.String())
	}
}
