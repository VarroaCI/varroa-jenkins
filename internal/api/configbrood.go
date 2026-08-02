package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// CatalogItemFilter narrows ListCatalogItems results: Type and Source are
// exact matches on the summary's Type/SourceRef; Q is a case-insensitive
// substring match over name, display name, description, and tags. Empty
// fields are unset; set fields combine with AND.
type CatalogItemFilter struct {
	Type   string
	Source string
	Q      string
}

// Timeout constants for ConfigBrood operations (DP3 execution split).
const (
	configListTimeout         = 3 * time.Second
	configGetTimeout          = 3 * time.Second
	configCreateUpdateTimeout = 30 * time.Second
	configDeleteTimeout       = 10 * time.Second
	configPauseResumeTimeout  = 10 * time.Second
	configSyncTimeout         = 10 * time.Second
	configComposeTimeout      = 60 * time.Second
)

// ConfigBrood is the abstract interface for config CRUD across clusters (§3).
// Reads (list/get) go direct-k8s for local and bus for remote.
// Mutations and compose go over the bus uniformly for all clusters.
type ConfigBrood interface {
	// --- ComposedBundles ---
	ListComposedBundles(ctx context.Context, cluster, ns string) ([]json.RawMessage, error)
	GetComposedBundle(ctx context.Context, cluster, ns, name string) (json.RawMessage, error)
	CreateComposedBundle(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error)
	UpdateComposedBundle(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error)
	DeleteComposedBundle(ctx context.Context, cluster, ns, name string) error
	PauseComposedBundle(ctx context.Context, cluster, ns, name string, paused bool) error
	ComposeBundle(ctx context.Context, cluster, ns string, spec json.RawMessage) (*bus.BundleComposePreview, error)

	// --- CatalogItems (read-only) ---
	ListCatalogItems(ctx context.Context, cluster, ns string, filter CatalogItemFilter) ([]json.RawMessage, string, error)
	GetCatalogItem(ctx context.Context, cluster, ns, name string) (json.RawMessage, error)

	// --- CatalogSources ---
	ListCatalogSources(ctx context.Context, cluster, ns string) ([]json.RawMessage, error)
	GetCatalogSource(ctx context.Context, cluster, ns, name string) (json.RawMessage, error)
	CreateCatalogSource(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error)
	UpdateCatalogSource(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error)
	DeleteCatalogSource(ctx context.Context, cluster, ns, name string) error
	SyncCatalogSource(ctx context.Context, cluster, ns, name string) error

	// --- JenkinsRoles (cluster-scoped) ---
	ListJenkinsRoles(ctx context.Context, cluster string) ([]json.RawMessage, error)
	GetJenkinsRole(ctx context.Context, cluster, name string) (json.RawMessage, error)
	CreateJenkinsRole(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error)
	UpdateJenkinsRole(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error)
	DeleteJenkinsRole(ctx context.Context, cluster, name string) error

	// --- JenkinsRoleBindings (cluster-scoped) ---
	ListJenkinsRoleBindings(ctx context.Context, cluster string) ([]json.RawMessage, error)
	GetJenkinsRoleBinding(ctx context.Context, cluster, name string) (json.RawMessage, error)
	CreateJenkinsRoleBinding(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error)
	UpdateJenkinsRoleBinding(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error)
	DeleteJenkinsRoleBinding(ctx context.Context, cluster, name string) error

	// --- ProvisioningDefaults + JenkinsVersionProfiles (cluster-scoped) ---
	GetProvisioningDefaults(ctx context.Context, cluster, name string) (json.RawMessage, error)
	UpdateProvisioningDefaults(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error)
	ListVersionProfiles(ctx context.Context, cluster string) ([]json.RawMessage, error)
	GetVersionProfile(ctx context.Context, cluster, name string) (json.RawMessage, error)
	CreateVersionProfile(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error)
	UpdateVersionProfile(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error)
	DeleteVersionProfile(ctx context.Context, cluster, name string) error
	ViewVersionProfiles(ctx context.Context, cluster string) ([]bus.VersionProfileView, error)
}

// busConfigBrood implements ConfigBrood over the NATS bus (with direct k8s
// for local reads).
type busConfigBrood struct {
	localCluster string
	conn         *bus.Conn
	client       controller.ResourceClient
	store        crdstore.Backend
	logger       *slog.Logger

	// request is the injectable NATS request function; defaults to conn.Request.
	// Override in tests to avoid needing a live NATS connection.
	request func(subject string, data []byte, timeout time.Duration) ([]byte, error)
}

// NewBusConfigBrood creates a new bus-backed ConfigBrood.
func NewBusConfigBrood(localCluster string, conn *bus.Conn, client controller.ResourceClient, store crdstore.Backend, logger *slog.Logger) ConfigBrood {
	return &busConfigBrood{
		localCluster: localCluster,
		conn:         conn,
		client:       client,
		store:        store,
		logger:       logger.With("component", "bus_config_brood"),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			resp, err := conn.Request(subject, data, timeout)
			if err != nil {
				return nil, err
			}
			return resp.Data, nil
		},
	}
}

func (f *busConfigBrood) requestTo(subject string, req any, timeout time.Duration) ([]byte, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	resp, err := f.request(subject, data, timeout)
	if err != nil {
		return nil, &ErrClusterUnreachable{Cluster: f.localCluster, Err: err}
	}
	return resp, nil
}

func (f *busConfigBrood) codeToErr(code, msg string) error {
	if code == "" {
		return nil
	}
	return &BroodError{Code: code, Msg: msg}
}

// isLocal returns true when the target cluster is the BFF's own cluster.
func (f *busConfigBrood) isLocal(cluster string) bool {
	return cluster == f.localCluster
}

// --- Read methods: local = direct k8s, remote = bus ---

func (f *busConfigBrood) ListComposedBundles(ctx context.Context, cluster, ns string) ([]json.RawMessage, error) {
	if f.isLocal(cluster) {
		items, err := crdstore.List[v1alpha1.ComposedBundle](ctx, f.store, ns, "")
		if err != nil {
			return nil, err
		}
		raw := make([]json.RawMessage, len(items))
		for i, cr := range items {
			r, err := json.Marshal(cr)
			if err != nil {
				return nil, fmt.Errorf("marshal bundle: %w", err)
			}
			raw[i] = r
		}
		return raw, nil
	}
	req := bus.ConfigListRequest{Namespace: ns}
	respData, err := f.requestTo(bus.OperatorBundlesSubject(cluster, "list"), req, configListTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigListResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("list bundles decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Items, nil
}

func (f *busConfigBrood) GetComposedBundle(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	if f.isLocal(cluster) {
		cr, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, f.store, name, ns)
		if err != nil {
			return nil, err
		}
		return json.Marshal(cr)
	}
	req := bus.ConfigGetRequest{Namespace: ns, Name: name}
	respData, err := f.requestTo(bus.OperatorBundlesSubject(cluster, "get"), req, configGetTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigGetResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("get bundle decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) ListCatalogItems(ctx context.Context, cluster, ns string, filter CatalogItemFilter) ([]json.RawMessage, string, error) {
	var items []json.RawMessage
	var operatorNs string
	if f.isLocal(cluster) {
		crds, err := crdstore.List[v1alpha1.CatalogItem](ctx, f.store, ns, "")
		if err != nil {
			return nil, "", err
		}
		items = make([]json.RawMessage, len(crds))
		for i, cr := range crds {
			r, err := json.Marshal(controller.ProjectCatalogItemSummary(cr))
			if err != nil {
				return nil, "", fmt.Errorf("marshal catalogitem: %w", err)
			}
			items[i] = r
		}
	} else {
		req := bus.ConfigListRequest{Namespace: ns}
		respData, err := f.requestTo(bus.OperatorCatalogSubject(cluster, "itemlist"), req, configListTimeout)
		if err != nil {
			return nil, "", err
		}
		var resp bus.ConfigListResponse
		if err := json.Unmarshal(respData, &resp); err != nil {
			return nil, "", fmt.Errorf("list items decode: %w", err)
		}
		if resp.Code != "" {
			return nil, "", f.codeToErr(resp.Code, resp.Error)
		}
		items, operatorNs = resp.Items, resp.OperatorNamespace
	}
	if filter == (CatalogItemFilter{}) {
		return items, operatorNs, nil
	}
	kept := make([]json.RawMessage, 0, len(items))
	for i, raw := range items {
		var summary bus.CatalogItemSummary
		if err := json.Unmarshal(raw, &summary); err != nil {
			return nil, "", fmt.Errorf("catalog item index %d namespace %q: invalid summary: %w", i, ns, err)
		}
		if (filter.Type == "" || summary.Type == filter.Type) &&
			(filter.Source == "" || summary.SourceRef == filter.Source) &&
			(filter.Q == "" || matchCatalogItemQ(summary, filter.Q)) {
			kept = append(kept, raw)
		}
	}
	return kept, operatorNs, nil
}

func matchCatalogItemQ(summary bus.CatalogItemSummary, q string) bool {
	q = strings.ToLower(q)
	fields := make([]string, 0, 3+len(summary.Tags))
	fields = append(fields, summary.Name, summary.DisplayName, summary.Description)
	fields = append(fields, summary.Tags...)
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}
	return false
}

func (f *busConfigBrood) GetCatalogItem(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	if f.isLocal(cluster) {
		cr, err := crdstore.Get[v1alpha1.CatalogItem](ctx, f.store, name, ns)
		if err != nil {
			return nil, err
		}
		return json.Marshal(cr)
	}
	req := bus.ConfigGetRequest{Namespace: ns, Name: name}
	respData, err := f.requestTo(bus.OperatorCatalogSubject(cluster, "itemget"), req, configGetTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigGetResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("get item decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) ListCatalogSources(ctx context.Context, cluster, ns string) ([]json.RawMessage, error) {
	if f.isLocal(cluster) {
		items, err := crdstore.List[v1alpha1.CatalogSource](ctx, f.store, ns, "")
		if err != nil {
			return nil, err
		}
		raw := make([]json.RawMessage, len(items))
		for i, cr := range items {
			r, err := json.Marshal(cr)
			if err != nil {
				return nil, fmt.Errorf("marshal catalogsource: %w", err)
			}
			raw[i] = r
		}
		return raw, nil
	}
	req := bus.ConfigListRequest{Namespace: ns}
	respData, err := f.requestTo(bus.OperatorCatalogSubject(cluster, "sourcelist"), req, configListTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigListResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("list sources decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Items, nil
}

func (f *busConfigBrood) GetCatalogSource(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	if f.isLocal(cluster) {
		cr, err := crdstore.Get[v1alpha1.CatalogSource](ctx, f.store, name, ns)
		if err != nil {
			return nil, err
		}
		return json.Marshal(cr)
	}
	req := bus.ConfigGetRequest{Namespace: ns, Name: name}
	respData, err := f.requestTo(bus.OperatorCatalogSubject(cluster, "sourceget"), req, configGetTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigGetResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("get source decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) ListJenkinsRoles(ctx context.Context, cluster string) ([]json.RawMessage, error) {
	if f.isLocal(cluster) {
		items, err := crdstore.List[v1alpha1.JenkinsRole](ctx, f.store, "", "")
		if err != nil {
			return nil, err
		}
		raw := make([]json.RawMessage, len(items))
		for i, cr := range items {
			r, err := json.Marshal(cr)
			if err != nil {
				return nil, fmt.Errorf("marshal jenkinsrole: %w", err)
			}
			raw[i] = r
		}
		return raw, nil
	}
	req := bus.ConfigListRequest{}
	respData, err := f.requestTo(bus.OperatorRbacSubject(cluster, "rolelist"), req, configListTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigListResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("list roles decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Items, nil
}

func (f *busConfigBrood) GetJenkinsRole(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	if f.isLocal(cluster) {
		cr, err := crdstore.Get[v1alpha1.JenkinsRole](ctx, f.store, name, "")
		if err != nil {
			return nil, err
		}
		return json.Marshal(cr)
	}
	req := bus.ConfigGetRequest{Name: name}
	respData, err := f.requestTo(bus.OperatorRbacSubject(cluster, "roleget"), req, configGetTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigGetResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("get role decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) ListJenkinsRoleBindings(ctx context.Context, cluster string) ([]json.RawMessage, error) {
	if f.isLocal(cluster) {
		items, err := crdstore.List[v1alpha1.JenkinsRoleBinding](ctx, f.store, "", "")
		if err != nil {
			return nil, err
		}
		raw := make([]json.RawMessage, len(items))
		for i, cr := range items {
			r, err := json.Marshal(cr)
			if err != nil {
				return nil, fmt.Errorf("marshal jenkinsrolebinding: %w", err)
			}
			raw[i] = r
		}
		return raw, nil
	}
	req := bus.ConfigListRequest{}
	respData, err := f.requestTo(bus.OperatorRbacSubject(cluster, "bindinglist"), req, configListTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigListResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("list bindings decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Items, nil
}

func (f *busConfigBrood) GetJenkinsRoleBinding(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	if f.isLocal(cluster) {
		cr, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](ctx, f.store, name, "")
		if err != nil {
			return nil, err
		}
		return json.Marshal(cr)
	}
	req := bus.ConfigGetRequest{Name: name}
	respData, err := f.requestTo(bus.OperatorRbacSubject(cluster, "bindingget"), req, configGetTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigGetResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("get binding decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) GetProvisioningDefaults(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	if f.isLocal(cluster) {
		cr, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, f.store, name, "")
		if err != nil {
			return nil, err
		}
		return json.Marshal(cr)
	}
	req := bus.ConfigGetRequest{Name: name}
	respData, err := f.requestTo(bus.OperatorProvisioningDefaultsSubject(cluster, "get"), req, configGetTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigGetResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("get provisioningdefaults decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) ListVersionProfiles(ctx context.Context, cluster string) ([]json.RawMessage, error) {
	if f.isLocal(cluster) {
		items, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, f.store, "", "")
		if err != nil {
			return nil, err
		}
		raw := make([]json.RawMessage, len(items))
		for i, cr := range items {
			r, err := json.Marshal(cr)
			if err != nil {
				return nil, fmt.Errorf("marshal jenkinsversionprofile: %w", err)
			}
			raw[i] = r
		}
		return raw, nil
	}
	req := bus.ConfigListRequest{}
	respData, err := f.requestTo(bus.OperatorVersionProfilesSubject(cluster, "list"), req, configListTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigListResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("list versionprofiles decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Items, nil
}

func (f *busConfigBrood) GetVersionProfile(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	if f.isLocal(cluster) {
		cr, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, f.store, name, "")
		if err != nil {
			return nil, err
		}
		return json.Marshal(cr)
	}
	req := bus.ConfigGetRequest{Name: name}
	respData, err := f.requestTo(bus.OperatorVersionProfilesSubject(cluster, "get"), req, configGetTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigGetResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("get versionprofile decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) ViewVersionProfiles(ctx context.Context, cluster string) ([]bus.VersionProfileView, error) {
	req := bus.ConfigListRequest{}
	respData, err := f.requestTo(bus.OperatorVersionProfilesSubject(cluster, "view"), req, configListTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.VersionProfileViewResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("view versionprofiles decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Profiles, nil
}

// --- Mutation methods: all go over the bus uniformly ---

func (f *busConfigBrood) UpdateProvisioningDefaults(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	req := bus.ConfigApplyRequest{Name: name, Object: obj}
	respData, err := f.requestTo(bus.OperatorProvisioningDefaultsSubject(cluster, "update"), req, configCreateUpdateTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("update provisioningdefaults decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) CreateVersionProfile(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	req := bus.ConfigApplyRequest{Name: name, Object: obj}
	respData, err := f.requestTo(bus.OperatorVersionProfilesSubject(cluster, "create"), req, configCreateUpdateTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("create versionprofile decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) UpdateVersionProfile(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	req := bus.ConfigApplyRequest{Name: name, Object: obj}
	respData, err := f.requestTo(bus.OperatorVersionProfilesSubject(cluster, "update"), req, configCreateUpdateTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("update versionprofile decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) DeleteVersionProfile(ctx context.Context, cluster, name string) error {
	req := bus.ConfigDeleteRequest{Name: name}
	respData, err := f.requestTo(bus.OperatorVersionProfilesSubject(cluster, "delete"), req, configDeleteTimeout)
	if err != nil {
		return err
	}
	var resp bus.ConfigDeleteResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return fmt.Errorf("delete versionprofile decode: %w", err)
	}
	return f.codeToErr(resp.Code, resp.Error)
}

func (f *busConfigBrood) CreateComposedBundle(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	req := bus.ConfigApplyRequest{Namespace: ns, Name: name, Object: obj}
	respData, err := f.requestTo(bus.OperatorBundlesSubject(cluster, "create"), req, configCreateUpdateTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("create bundle decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) UpdateComposedBundle(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	req := bus.ConfigApplyRequest{Namespace: ns, Name: name, Object: obj}
	respData, err := f.requestTo(bus.OperatorBundlesSubject(cluster, "update"), req, configCreateUpdateTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("update bundle decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) DeleteComposedBundle(ctx context.Context, cluster, ns, name string) error {
	req := bus.ConfigDeleteRequest{Namespace: ns, Name: name}
	respData, err := f.requestTo(bus.OperatorBundlesSubject(cluster, "delete"), req, configDeleteTimeout)
	if err != nil {
		return err
	}
	var resp bus.ConfigDeleteResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return fmt.Errorf("delete bundle decode: %w", err)
	}
	return f.codeToErr(resp.Code, resp.Error)
}

func (f *busConfigBrood) PauseComposedBundle(ctx context.Context, cluster, ns, name string, paused bool) error {
	subject := bus.OperatorBundlesSubject(cluster, "pause")
	if !paused {
		subject = bus.OperatorBundlesSubject(cluster, "resume")
	}
	req := bus.BundlePauseRequest{Namespace: ns, Name: name, Paused: paused}
	respData, err := f.requestTo(subject, req, configPauseResumeTimeout)
	if err != nil {
		return err
	}
	var resp bus.BundlePauseResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return fmt.Errorf("pause bundle decode: %w", err)
	}
	return f.codeToErr(resp.Code, resp.Error)
}

func (f *busConfigBrood) CreateCatalogSource(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	req := bus.ConfigApplyRequest{Namespace: ns, Name: name, Object: obj}
	respData, err := f.requestTo(bus.OperatorCatalogSubject(cluster, "sourcecreate"), req, configCreateUpdateTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("create source decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) UpdateCatalogSource(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	req := bus.ConfigApplyRequest{Namespace: ns, Name: name, Object: obj}
	respData, err := f.requestTo(bus.OperatorCatalogSubject(cluster, "sourceupdate"), req, configCreateUpdateTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("update source decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) DeleteCatalogSource(ctx context.Context, cluster, ns, name string) error {
	req := bus.ConfigDeleteRequest{Namespace: ns, Name: name}
	respData, err := f.requestTo(bus.OperatorCatalogSubject(cluster, "sourcedelete"), req, configDeleteTimeout)
	if err != nil {
		return err
	}
	var resp bus.ConfigDeleteResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return fmt.Errorf("delete source decode: %w", err)
	}
	return f.codeToErr(resp.Code, resp.Error)
}

func (f *busConfigBrood) SyncCatalogSource(ctx context.Context, cluster, ns, name string) error {
	req := bus.CatalogSyncRequest{Namespace: ns, Name: name}
	respData, err := f.requestTo(bus.OperatorCatalogSubject(cluster, "sourcesync"), req, configSyncTimeout)
	if err != nil {
		return err
	}
	var resp bus.CatalogSyncResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return fmt.Errorf("sync source decode: %w", err)
	}
	return f.codeToErr(resp.Code, resp.Error)
}

func (f *busConfigBrood) CreateJenkinsRole(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	req := bus.ConfigApplyRequest{Name: name, Object: obj}
	respData, err := f.requestTo(bus.OperatorRbacSubject(cluster, "rolecreate"), req, configCreateUpdateTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("create role decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) UpdateJenkinsRole(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	req := bus.ConfigApplyRequest{Name: name, Object: obj}
	respData, err := f.requestTo(bus.OperatorRbacSubject(cluster, "roleupdate"), req, configCreateUpdateTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("update role decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) DeleteJenkinsRole(ctx context.Context, cluster, name string) error {
	req := bus.ConfigDeleteRequest{Name: name}
	respData, err := f.requestTo(bus.OperatorRbacSubject(cluster, "roledelete"), req, configDeleteTimeout)
	if err != nil {
		return err
	}
	var resp bus.ConfigDeleteResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return fmt.Errorf("delete role decode: %w", err)
	}
	return f.codeToErr(resp.Code, resp.Error)
}

func (f *busConfigBrood) CreateJenkinsRoleBinding(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	req := bus.ConfigApplyRequest{Name: name, Object: obj}
	respData, err := f.requestTo(bus.OperatorRbacSubject(cluster, "bindingcreate"), req, configCreateUpdateTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("create binding decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) UpdateJenkinsRoleBinding(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	req := bus.ConfigApplyRequest{Name: name, Object: obj}
	respData, err := f.requestTo(bus.OperatorRbacSubject(cluster, "bindingupdate"), req, configCreateUpdateTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.ConfigApplyResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("update binding decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Item, nil
}

func (f *busConfigBrood) DeleteJenkinsRoleBinding(ctx context.Context, cluster, name string) error {
	req := bus.ConfigDeleteRequest{Name: name}
	respData, err := f.requestTo(bus.OperatorRbacSubject(cluster, "bindingdelete"), req, configDeleteTimeout)
	if err != nil {
		return err
	}
	var resp bus.ConfigDeleteResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return fmt.Errorf("delete binding decode: %w", err)
	}
	return f.codeToErr(resp.Code, resp.Error)
}

// ComposeBundle goes over the bus uniformly for all clusters with 60s timeout.
func (f *busConfigBrood) ComposeBundle(ctx context.Context, cluster, ns string, spec json.RawMessage) (*bus.BundleComposePreview, error) {
	req := bus.BundleComposeRequest{Namespace: ns, Spec: spec}
	respData, err := f.requestTo(bus.OperatorBundlesSubject(cluster, "preview"), req, configComposeTimeout)
	if err != nil {
		return nil, err
	}
	var resp bus.BundleComposeResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		return nil, fmt.Errorf("compose decode: %w", err)
	}
	if resp.Code != "" {
		return nil, f.codeToErr(resp.Code, resp.Error)
	}
	return resp.Preview, nil
}
