package controller

import (
	"context"
	"encoding/json"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// HandleAssistantConfigUpdate handles operator.<cluster>.assistantconfig.update.
// The object name is always v1alpha1.AssistantConfigName. A request with a
// resourceVersion updates only that version. A request without one only
// creates the object, and an existing object answers conflict. The object is
// validated before the write. OnAssistantConfigChange runs after a stored write.
func (c *ConfigCRUD) HandleAssistantConfigUpdate(data []byte) []byte {
	var req bus.ConfigApplyRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cr v1alpha1.AssistantConfig
	if err := json.Unmarshal(req.Object, &cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "invalid assistantconfig JSON", Code: bus.CodeInvalid})
	}
	cr.Name = v1alpha1.AssistantConfigName
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = v1alpha1.AssistantConfigKind
	if err := v1alpha1.ValidateAssistantConfig(&cr); err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: err.Error(), Code: bus.CodeInvalid})
	}
	createOnly := cr.ResourceVersion == ""
	if err := crdstore.ApplyAt[v1alpha1.AssistantConfig](ctx, c.Store, &cr); err != nil {
		if apierrors.IsConflict(err) {
			return c.configReply(bus.ConfigApplyResponse{Error: crdstore.ApplyAtConflictMessage("assistantconfig", cr.Name, createOnly), Code: bus.CodeConflict})
		}
		return c.configReply(bus.ConfigApplyResponse{Error: err.Error(), Code: k8sConfigErrToCode(err)})
	}
	if c.OnAssistantConfigChange != nil {
		c.OnAssistantConfigChange()
	}
	// The write does not return the stored status: the status subresource drops
	// what the client sent. The reply reads the object back, so it holds the
	// stored status.
	// The write is committed. A failed read answers the object that was sent,
	// so the caller does not see an error for a change that was stored.
	out := &cr
	if stored, gerr := crdstore.Get[v1alpha1.AssistantConfig](ctx, c.Store, cr.Name, ""); gerr == nil {
		out = stored
	} else {
		// The submitted status is discarded by the apiserver, so it is not echoed.
		cr.Status = v1alpha1.AssistantConfigStatus{}
		c.Logger.Warn("assistantconfig read-back failed after a stored write", "error", gerr)
	}
	raw, err := marshalCR(out)
	if err != nil {
		return c.configReply(bus.ConfigApplyResponse{Error: "marshal error", Code: bus.CodeInternal})
	}
	return c.configReply(bus.ConfigApplyResponse{Item: raw})
}
