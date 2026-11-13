package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// Error codes of the AssistantConfig routes and tools.
const (
	assistantCfgErrNotFound    = "not_found"
	assistantCfgErrConflict    = "conflict"
	assistantCfgErrInvalid     = "invalid_config"
	assistantCfgErrUnavailable = "operator_unavailable"
	assistantCfgErrTimeout     = "operator_timeout"
	assistantCfgErrNoOperator  = "the assistant config is not served by this BFF"
)

// AssistantConfigError is a refusal of an AssistantConfig read or write. Status
// is the HTTP status; Code names the failure.
type AssistantConfigError struct {
	Status  int
	Code    string
	Message string
}

func (e *AssistantConfigError) Error() string { return e.Message }

// DefaultAssistantConfigObject returns the object that GET answers when the
// stored object does not exist: no provider, the default limits, and no
// resourceVersion.
func DefaultAssistantConfigObject() *v1alpha1.AssistantConfig {
	return &v1alpha1.AssistantConfig{
		TypeMeta:   metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: v1alpha1.AssistantConfigKind},
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.AssistantConfigName},
		Spec:       v1alpha1.DefaultAssistantConfigSpec(),
	}
}

// GetConfig reads the AssistantConfig through the operator store. An absent
// object answers the default object.
func (rt *AssistantRuntime) GetConfig(ctx context.Context) (*v1alpha1.AssistantConfig, *AssistantConfigError) {
	if rt.Operator == nil {
		return nil, &AssistantConfigError{Status: http.StatusServiceUnavailable, Code: assistantCfgErrUnavailable, Message: assistantCfgErrNoOperator}
	}
	obj, err := rt.Operator.GetAssistantConfig(ctx, rt.Cluster)
	if err != nil {
		return nil, assistantConfigFailure(err)
	}
	if obj == nil {
		return DefaultAssistantConfigObject(), nil
	}
	return obj, nil
}

// PutConfig sends the object to the operator of this cluster. The operator
// validates it and applies it with its resourceVersion fence. A reply holds
// the stored object.
func (rt *AssistantRuntime) PutConfig(ctx context.Context, obj *v1alpha1.AssistantConfig) (*v1alpha1.AssistantConfig, *AssistantConfigError) {
	if rt.Operator == nil {
		return nil, &AssistantConfigError{Status: http.StatusServiceUnavailable, Code: assistantCfgErrUnavailable, Message: assistantCfgErrNoOperator}
	}
	out, err := rt.Operator.UpdateAssistantConfig(ctx, rt.Cluster, obj)
	if err != nil {
		return nil, assistantConfigFailure(err)
	}
	return out, nil
}

// assistantConfigFailure maps an operator or bus error to a refusal. A reply
// code keeps its meaning: not_found is 404, conflict 409, and invalid 400. A
// request that got no answer is 503, and one that timed out is 504.
func assistantConfigFailure(err error) *AssistantConfigError {
	var fe *BroodError
	if errors.As(err, &fe) {
		switch fe.Code {
		case bus.CodeNotFound:
			return &AssistantConfigError{Status: http.StatusNotFound, Code: assistantCfgErrNotFound, Message: fe.Msg}
		case bus.CodeConflict:
			return &AssistantConfigError{Status: http.StatusConflict, Code: assistantCfgErrConflict, Message: fe.Msg}
		case bus.CodeInvalid:
			return &AssistantConfigError{Status: http.StatusBadRequest, Code: assistantCfgErrInvalid, Message: fe.Msg}
		}
		return &AssistantConfigError{Status: http.StatusInternalServerError, Code: "internal", Message: fe.Msg}
	}
	var unreachable *ErrClusterUnreachable
	if errors.As(err, &unreachable) {
		if errors.Is(unreachable.Err, nats.ErrTimeout) {
			return &AssistantConfigError{Status: http.StatusGatewayTimeout, Code: assistantCfgErrTimeout, Message: "the operator did not answer in time"}
		}
		return &AssistantConfigError{Status: http.StatusServiceUnavailable, Code: assistantCfgErrUnavailable, Message: "the operator is unavailable"}
	}
	return &AssistantConfigError{Status: http.StatusInternalServerError, Code: "internal", Message: fmt.Sprintf("assistant config: %v", err)}
}
