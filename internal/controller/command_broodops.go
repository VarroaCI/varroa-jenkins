package controller

import (
	"context"
	"encoding/json"
	"log/slog"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// CommandBroodOps implements operator-side brood-op command handlers for
// BFF-initiated brood operation commands received over the NATS bus.
// Each Handle* method unmarshals the request, validates against local k8s
// state, and returns a JSON-marshalled response (§3).
type CommandBroodOps struct {
	client.Client
	OperatorNamespace string
	Logger            *slog.Logger
}

// NewCommandBroodOps creates a new CommandBroodOps.
func NewCommandBroodOps(c client.Client, operatorNS string, logger *slog.Logger) *CommandBroodOps {
	return &CommandBroodOps{
		Client:            c,
		OperatorNamespace: operatorNS,
		Logger:            logger.With("component", "command_broodops"),
	}
}

// broodOpsReply marshals v to JSON for the bus response.
func (c *CommandBroodOps) broodOpsReply(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		c.Logger.Error("broodops reply marshal failed", "error", err)
		errResp, _ := json.Marshal(map[string]string{"error": "internal marshal error", "code": bus.CodeInternal})
		return errResp
	}
	return data
}

// broodStripManagedFields removes managedFields from a BroodOperation.
func broodStripManagedFields(op *v1alpha1.BroodOperation) {
	op.ManagedFields = nil
}

// HandleCreate handles operator.<cluster>.broodops.create.
func (c *CommandBroodOps) HandleCreate(data []byte) []byte {
	var req bus.BroodOpsCreateRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeInvalid, Error: "invalid request"})
	}

	ctx := context.Background()

	// Unmarshal spec.
	var spec v1alpha1.BroodOperationSpec
	if err := json.Unmarshal(req.Spec, &spec); err != nil {
		return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeInvalid, Error: "invalid spec"})
	}

	// Validate brood tenancy.
	if err := ValidateBroodTenancy(spec, req.Namespace, c.OperatorNamespace); err != nil {
		return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeInvalid, Error: err.Error()})
	}

	// Build the BroodOperation CR.
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
		},
		Spec: spec,
	}

	// Create the CR.
	if err := c.Create(ctx, op); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeConflict, Error: "already exists"})
		}
		c.Logger.Error("broodops create failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeInternal, Error: err.Error()})
	}

	// Stamp startedBy via merge-patch status update.
	if req.StartedBy != "" {
		patchJSON, marshalErr := json.Marshal(map[string]interface{}{
			"status": map[string]string{
				"startedBy": req.StartedBy,
			},
		})
		if marshalErr == nil {
			if err := c.Status().Patch(context.Background(), op, client.RawPatch(types.MergePatchType, patchJSON)); err != nil {
				c.Logger.Warn("failed to stamp startedBy on brood operation", "namespace", req.Namespace, "name", req.Name, "error", err)
			}
		}
	}

	// Reply with the created CR.
	broodStripManagedFields(op)
	raw, err := json.Marshal(op)
	if err != nil {
		return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeInternal, Error: "marshal error"})
	}
	return c.broodOpsReply(bus.BroodOpsOpResponse{Op: raw})
}

// HandleGet handles operator.<cluster>.broodops.get.
func (c *CommandBroodOps) HandleGet(data []byte) []byte {
	var req bus.BroodOpsGetRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeInvalid, Error: "invalid request"})
	}

	ctx := context.Background()

	var op v1alpha1.BroodOperation
	if err := c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &op); err != nil {
		if k8serrors.IsNotFound(err) {
			return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeNotFound, Error: "not found"})
		}
		c.Logger.Error("broodops get failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeInternal, Error: err.Error()})
	}

	broodStripManagedFields(&op)
	raw, err := json.Marshal(op)
	if err != nil {
		return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeInternal, Error: "marshal error"})
	}
	return c.broodOpsReply(bus.BroodOpsOpResponse{Op: raw})
}

// HandleList handles operator.<cluster>.broodops.list.
func (c *CommandBroodOps) HandleList(data []byte) []byte {
	var req bus.BroodOpsListRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.broodOpsReply(bus.BroodOpsListResponse{Code: bus.CodeInvalid, Error: "invalid request"})
	}

	ctx := context.Background()

	var list v1alpha1.BroodOperationList
	var listOpts []client.ListOption
	if req.Namespace != "" {
		listOpts = append(listOpts, client.InNamespace(req.Namespace))
	}
	if err := c.List(ctx, &list, listOpts...); err != nil {
		c.Logger.Error("broodops list failed", "namespace", req.Namespace, "error", err)
		return c.broodOpsReply(bus.BroodOpsListResponse{Code: bus.CodeInternal, Error: err.Error()})
	}

	// Strip managedFields and marshal each CR.
	rawItems := make([]json.RawMessage, 0, len(list.Items))
	for i := range list.Items {
		broodStripManagedFields(&list.Items[i])
		raw, err := json.Marshal(list.Items[i])
		if err != nil {
			c.Logger.Error("marshal broodop failed", "name", list.Items[i].Name, "namespace", list.Items[i].Namespace, "error", err)
			return c.broodOpsReply(bus.BroodOpsListResponse{Code: bus.CodeInternal, Error: "marshal error"})
		}
		rawItems = append(rawItems, raw)
	}

	// Check size budget.
	reply, err := json.Marshal(bus.BroodOpsListResponse{Ops: rawItems})
	if err != nil {
		c.Logger.Error("broodops list reply marshal failed", "error", err)
		return c.broodOpsReply(bus.BroodOpsListResponse{Code: bus.CodeInternal, Error: "marshal error"})
	}
	if len(reply) > 900*1024 {
		return c.broodOpsReply(bus.BroodOpsListResponse{Code: bus.CodeInternal, Error: "list too large"})
	}
	return reply
}

// HandleCancel handles operator.<cluster>.broodops.cancel.
func (c *CommandBroodOps) HandleCancel(data []byte) []byte {
	var req bus.BroodOpsCancelRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.broodOpsReply(bus.BroodOpsCancelResponse{Code: bus.CodeInvalid, Error: "invalid request"})
	}

	ctx := context.Background()

	var op v1alpha1.BroodOperation
	if err := c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &op); err != nil {
		if k8serrors.IsNotFound(err) {
			return c.broodOpsReply(bus.BroodOpsCancelResponse{Code: bus.CodeNotFound, Error: "not found"})
		}
		c.Logger.Error("broodops cancel get failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.broodOpsReply(bus.BroodOpsCancelResponse{Code: bus.CodeInternal, Error: err.Error()})
	}

	if err := c.Delete(ctx, &op); err != nil {
		c.Logger.Error("broodops cancel delete failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.broodOpsReply(bus.BroodOpsCancelResponse{Code: bus.CodeInternal, Error: err.Error()})
	}

	return c.broodOpsReply(bus.BroodOpsCancelResponse{})
}

// HandleSuspend handles operator.<cluster>.broodops.suspend.
func (c *CommandBroodOps) HandleSuspend(data []byte) []byte {
	var req bus.BroodOpsSuspendRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeInvalid, Error: "invalid request"})
	}

	ctx := context.Background()

	var op v1alpha1.BroodOperation
	if err := c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &op); err != nil {
		if k8serrors.IsNotFound(err) {
			return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeNotFound, Error: "not found"})
		}
		c.Logger.Error("broodops suspend get failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeInternal, Error: err.Error()})
	}

	op.Spec.Suspend = req.Suspend
	if err := c.Update(ctx, &op); err != nil {
		c.Logger.Error("broodops suspend update failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeInternal, Error: err.Error()})
	}

	broodStripManagedFields(&op)
	raw, err := json.Marshal(op)
	if err != nil {
		return c.broodOpsReply(bus.BroodOpsOpResponse{Code: bus.CodeInternal, Error: "marshal error"})
	}
	return c.broodOpsReply(bus.BroodOpsOpResponse{Op: raw})
}

// HandlePreview handles operator.<cluster>.broodops.preview.
func (c *CommandBroodOps) HandlePreview(data []byte) []byte {
	var req bus.BroodOpsPreviewRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.broodOpsReply(bus.BroodOpsPreviewResponse{Code: bus.CodeInvalid, Error: "invalid request"})
	}

	ctx := context.Background()

	var spec v1alpha1.BroodOperationSpec
	if err := json.Unmarshal(req.Spec, &spec); err != nil {
		return c.broodOpsReply(bus.BroodOpsPreviewResponse{Code: bus.CodeInvalid, Error: "invalid spec"})
	}

	targets, err := ResolveTargets(ctx, c.Client, spec, req.Namespace, c.OperatorNamespace)
	if err != nil {
		return c.broodOpsReply(bus.BroodOpsPreviewResponse{Code: bus.CodeInvalid, Error: err.Error()})
	}

	previewTargets := make([]bus.BroodPreviewTarget, 0, len(targets))
	for _, t := range targets {
		pt := bus.BroodPreviewTarget{
			Namespace:  t.Namespace,
			Name:       t.Name,
			Wave:       t.Wave,
			Applicable: t.Applicable,
		}
		if !t.Applicable {
			pt.Reason = t.SkipReason
		}
		previewTargets = append(previewTargets, pt)
	}

	return c.broodOpsReply(bus.BroodOpsPreviewResponse{Targets: previewTargets})
}
