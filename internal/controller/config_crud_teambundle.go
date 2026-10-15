package controller

import (
	"context"
	"encoding/json"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// teamBundleContentTimeout bounds one bundles.teamcontent read. It stays
// below the BFF's configGetTimeout, so the BFF does not report the member
// as unreachable while this operator still reads.
const teamBundleContentTimeout = 2 * time.Second

// teamBundleContentKeys lists the content keys a pipeline read of a
// member-placed Team needs. jenkins.yaml and plugins.yaml stay out of the
// reply, so it stays small.
var teamBundleContentKeys = []string{
	"items.yaml",
	"variables.yaml",
	"pipeline-templates.yaml",
	bundle.DroppedItemKeysConfigMapKey,
}

// teamHeld reports whether the Team's slot-0 cell holds the Team. It
// returns false, with no error, when the Team has no placement or the
// cell does not exist.
func (c *ConfigCRUD) teamHeld(ctx context.Context, team *v1alpha1.Team) (bool, error) {
	p := team.Status.Placement
	if p == nil || p.Cell == "" {
		return false, nil
	}
	cell, err := crdstore.Get[v1alpha1.Controller](ctx, c.Store, p.Cell, p.CellNamespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return v1alpha1.CellHoldsTenant(cell, team.Name), nil
}

// HandleBundlesTeamContent handles operator.<cluster>.bundles.teamcontent.
// It reads the Team, its pipelines bundle ref, the ComposedBundle, and the
// bundle's content ConfigMap from this cluster's own store.
func (c *ConfigCRUD) HandleBundlesTeamContent(data []byte) []byte {
	var req bus.TeamBundleContentRequest
	if err := json.Unmarshal(data, &req); err != nil || req.Team == "" {
		return c.configReply(bus.TeamBundleContentResponse{Code: bus.CodeInvalid, Error: "invalid request"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), teamBundleContentTimeout)
	defer cancel()

	team, err := crdstore.Get[v1alpha1.Team](ctx, c.Store, req.Team, "")
	if err != nil {
		return c.configReply(bus.TeamBundleContentResponse{Code: k8sConfigErrToCode(err), Error: err.Error()})
	}
	// The hold is read on every reply, so Held always reports the cell's
	// state for the Team, whether or not a bundle or its content exists.
	held, err := c.teamHeld(ctx, team)
	if err != nil {
		c.Logger.Error("get team cell failed", "team", req.Team, "error", err)
		return c.configReply(bus.TeamBundleContentResponse{Code: bus.CodeInternal, Error: err.Error()})
	}
	if team.Spec.Pipelines == nil || team.Spec.Pipelines.ComposedBundleRef == nil {
		return c.configReply(bus.TeamBundleContentResponse{Held: held})
	}
	ref := team.Spec.Pipelines.ComposedBundleRef

	cb, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, c.Store, ref.Name, ref.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.configReply(bus.TeamBundleContentResponse{Held: held})
		}
		c.Logger.Error("get team bundle failed", "team", req.Team, "error", err)
		return c.configReply(bus.TeamBundleContentResponse{Code: bus.CodeInternal, Error: err.Error()})
	}
	raw, err := marshalCR(cb)
	if err != nil {
		return c.configReply(bus.TeamBundleContentResponse{Code: bus.CodeInternal, Error: "marshal error"})
	}
	if cb.Status.ContentRef == "" {
		return c.configReply(bus.TeamBundleContentResponse{Bundle: raw, Held: held})
	}

	cm, err := c.Client.GetConfigMap(ctx, cb.Status.ContentRef, ref.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.configReply(bus.TeamBundleContentResponse{Bundle: raw, Held: held})
		}
		c.Logger.Error("get team bundle content failed", "team", req.Team, "error", err)
		return c.configReply(bus.TeamBundleContentResponse{Code: bus.CodeInternal, Error: err.Error()})
	}

	content := make(map[string]string, len(teamBundleContentKeys))
	for _, key := range teamBundleContentKeys {
		if v, ok := cm[key]; ok {
			content[key] = v
		}
	}
	reply := bus.TeamBundleContentResponse{Bundle: raw, Content: content, Held: held}
	out, err := json.Marshal(reply)
	if err != nil {
		return c.configReply(bus.TeamBundleContentResponse{Code: bus.CodeInternal, Error: "marshal error"})
	}
	if len(out) > bus.ListPageBudget {
		return c.configReply(bus.TeamBundleContentResponse{Code: bus.CodeInternal, Error: "team bundle content exceeds the reply budget"})
	}
	return out
}
