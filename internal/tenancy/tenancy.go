// Package tenancy provides namespace classification and lifecycle management
// for the Varroa control plane. It is the D2 seam consumed by change 2.
package tenancy

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// NamespaceState is the classification of a target namespace w.r.t. the control plane.
type NamespaceState string

const (
	// NamespaceReady means the namespace exists AND the control plane holds RBAC there
	// (default mode: any existing ns; scoped mode: ns in the managed set or the release ns).
	NamespaceReady NamespaceState = "Ready"
	// NamespaceMissing means the namespace object does not exist (apiserver 404).
	NamespaceMissing NamespaceState = "Missing"
	// NamespaceUnmanaged means the namespace exists but is outside the managed set
	// in scoped mode, so operator/BFF core-resource RBAC does not reach it (C1).
	// Never returned in default mode.
	NamespaceUnmanaged NamespaceState = "Unmanaged"
)

// NamespaceReader is the read-only surface Classify needs. The preflight Deps
// interface embeds exactly this (GetNamespace), so preflight can classify without
// gaining any namespace-write verb.
type NamespaceReader interface {
	// GetNamespace returns the namespace, or a k8s IsNotFound error if it does not exist.
	GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error)
}

// NamespaceClient is the full surface Ensure needs (reader + writer).
// ClientsetClient implements it.
type NamespaceClient interface {
	NamespaceReader
	// CreateNamespace creates the namespace with the given labels.
	// Returns IsAlreadyExists if racing.
	CreateNamespace(ctx context.Context, name string, labels map[string]string) error
	// PatchNamespaceLabels merges labels onto an existing namespace
	// (strategic/merge patch on metadata.labels; never removes labels it did not set).
	PatchNamespaceLabels(ctx context.Context, name string, labels map[string]string) error
}

// ManagedSet is the set of namespaces the control plane has RBAC in.
// An empty set means cluster-wide mode: EVERY namespace is treated as managed
// (nothing is ever Unmanaged).
type ManagedSet struct {
	clusterWide bool                // true when the operator/BFF run without managedNamespaces scoping
	members     map[string]struct{} // explicit managed namespaces + the release namespace
}

// NewManagedSet builds the set from the MANAGED_NAMESPACES env value and the
// operator namespace. managedEnv is space/comma separated; empty ⇒ clusterWide=true.
// operatorNS is always added when non-empty and the set is scoped.
func NewManagedSet(managedEnv, operatorNS string) ManagedSet {
	tokens := strings.FieldsFunc(managedEnv, func(r rune) bool {
		return r == ' ' || r == ','
	})
	// Filter out empty tokens (though FieldsFunc already skips runs).
	var clean []string
	for _, t := range tokens {
		if t != "" {
			clean = append(clean, t)
		}
	}
	if len(clean) == 0 {
		return ManagedSet{clusterWide: true, members: nil}
	}
	members := make(map[string]struct{}, len(clean)+1)
	for _, t := range clean {
		members[t] = struct{}{}
	}
	if operatorNS != "" {
		members[operatorNS] = struct{}{}
	}
	return ManagedSet{clusterWide: false, members: members}
}

// Contains reports whether ns is managed. Always true in cluster-wide mode.
func (m ManagedSet) Contains(ns string) bool {
	if m.clusterWide {
		return true
	}
	_, ok := m.members[ns]
	return ok
}

// Namespaces returns the explicit managed namespace names, or nil in
// cluster-wide mode. Used to scope the operator's informer cache so that
// namespace-scoped RBAC (the per-namespace operator Role) is sufficient.
func (m ManagedSet) Namespaces() []string {
	if m.clusterWide {
		return nil
	}
	out := make([]string, 0, len(m.members))
	for ns := range m.members {
		out = append(out, ns)
	}
	return out
}

// Classifier holds the full NamespaceClient and ManagedSet for Ensure and
// the method wrapper for Classify.
type Classifier struct {
	client NamespaceClient
	set    ManagedSet
}

// NewClassifier creates a new Classifier.
func NewClassifier(c NamespaceClient, set ManagedSet) *Classifier {
	return &Classifier{client: c, set: set}
}

// Classify returns the NamespaceState for name. Errors are reserved for
// apiserver failures OTHER than NotFound (NotFound is a normal Missing result,
// not an error).
func Classify(ctx context.Context, r NamespaceReader, set ManagedSet, name string) (NamespaceState, error) {
	_, err := r.GetNamespace(ctx, name)
	switch {
	case err == nil:
		if set.Contains(name) {
			return NamespaceReady, nil
		}
		return NamespaceUnmanaged, nil
	case apierrors.IsNotFound(err):
		return NamespaceMissing, nil
	default:
		return "", err // transient apiserver error — caller degrades to a warn, not a hard fail
	}
}

// Classify is a method wrapper for holders of a full Classifier
// (operator gate, change-2 Ensure callers).
func (c *Classifier) Classify(ctx context.Context, name string) (NamespaceState, error) {
	return Classify(ctx, c.client, c.set, name)
}

// EnsureResult holds the outcome of an Ensure call.
type EnsureResult struct {
	Created bool           // true iff this call created the namespace
	State   NamespaceState // post-condition: Ready or Unmanaged (NEVER Missing)
	Managed bool           // == set.Contains(name); false ⇒ no operator RBAC yet (C1 warning)
}

// Ensure makes the namespace exist and carry (at least) the given labels.
// Idempotent: namespace absent → CreateNamespace; namespace present → PatchNamespaceLabels.
// Post-state is always Ready or Unmanaged. In scoped mode a namespace not in the
// managed set yields State=Unmanaged, Managed=false.
func (c *Classifier) Ensure(ctx context.Context, name string, labels map[string]string) (EnsureResult, error) {
	_, err := c.client.GetNamespace(ctx, name)
	switch {
	case apierrors.IsNotFound(err):
		if cerr := c.client.CreateNamespace(ctx, name, labels); cerr != nil {
			if apierrors.IsAlreadyExists(cerr) { // create/exists race
				if perr := c.client.PatchNamespaceLabels(ctx, name, labels); perr != nil {
					return EnsureResult{}, perr
				}
				return c.result(name, false), nil
			}
			return EnsureResult{}, cerr
		}
		return c.result(name, true), nil
	case err == nil:
		if perr := c.client.PatchNamespaceLabels(ctx, name, labels); perr != nil {
			return EnsureResult{}, perr
		}
		return c.result(name, false), nil
	default:
		return EnsureResult{}, err
	}
}

// result builds an EnsureResult from the post-condition state.
func (c *Classifier) result(name string, created bool) EnsureResult {
	managed := c.set.Contains(name)
	state := NamespaceReady
	if !managed {
		state = NamespaceUnmanaged
	}
	return EnsureResult{
		Created: created,
		State:   state,
		Managed: managed,
	}
}
