package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// fakeAssistantOperator holds one stored object and fences writes the way the
// operator does: a version must match, and a write without one only creates.
type fakeAssistantOperator struct {
	stored  *v1alpha1.AssistantConfig
	rv      int
	lastPut *v1alpha1.AssistantConfig
	err     error
	// keys records each key write by provider name. cleared records each clear.
	keys    map[string][]byte
	cleared []string
	keyErr  error
}

func (f *fakeAssistantOperator) GetAssistantConfig(context.Context, string) (*v1alpha1.AssistantConfig, error) {
	if f.stored == nil {
		return nil, nil
	}
	c := f.stored.DeepCopy()
	return c, nil
}

func (f *fakeAssistantOperator) UpdateAssistantConfig(_ context.Context, _ string, obj *v1alpha1.AssistantConfig) (*v1alpha1.AssistantConfig, error) {
	f.lastPut = obj.DeepCopy()
	if f.err != nil {
		return nil, f.err
	}
	if f.stored == nil && obj.ResourceVersion != "" {
		return nil, &BroodError{Code: bus.CodeConflict, Msg: "changed or was deleted"}
	}
	if f.stored != nil && obj.ResourceVersion != f.stored.ResourceVersion {
		return nil, &BroodError{Code: bus.CodeConflict, Msg: "changed or was deleted"}
	}
	if f.stored != nil && obj.ResourceVersion == "" {
		return nil, &BroodError{Code: bus.CodeConflict, Msg: "exists"}
	}
	f.rv++
	c := obj.DeepCopy()
	c.ResourceVersion = string(rune('0' + f.rv))
	f.stored = c
	return c.DeepCopy(), nil
}

func assistantConfigFixture(t *testing.T) (*assistantFixture, *fakeAssistantOperator, *recordingActivitySink) {
	t.Helper()
	f := newAssistantFixture(t)
	op := &fakeAssistantOperator{}
	f.rt.Operator = op
	sink := &recordingActivitySink{}
	f.deps.ActivityPublisher = sink
	return f, op, sink
}

func TestGetAssistantConfigAbsentAnswersDefaults(t *testing.T) {
	f, _, _ := assistantConfigFixture(t)
	rec := f.do(t, f.admin, http.MethodGet, "/assistant/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	var got v1alpha1.AssistantConfig
	decodeBody(t, rec, &got)
	if got.ResourceVersion != "" {
		t.Fatalf("absent object carries resourceVersion %q", got.ResourceVersion)
	}
	if len(got.Spec.Providers) != 0 || got.Spec.Budgets.Session != v1alpha1.AssistantDefaultSessionBudget {
		t.Fatalf("absent object is not the default spec: %+v", got.Spec)
	}
}

func TestAssistantConfigRoutesNeedAdmin(t *testing.T) {
	f, _, _ := assistantConfigFixture(t)
	if rec := f.do(t, f.viewer, http.MethodGet, "/assistant/config", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer GET: %d, want 403", rec.Code)
	}
	if rec := f.do(t, f.viewer, http.MethodPut, "/assistant/config", v1alpha1.AssistantConfig{}); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer PUT: %d, want 403", rec.Code)
	}
}

func TestAssistantConfigRoutesOffWithoutRuntime(t *testing.T) {
	f, _, _ := assistantConfigFixture(t)
	f.deps.Assistant = nil
	rec := f.do(t, f.admin, http.MethodGet, "/assistant/config", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET with the assistant off: %d, want 503", rec.Code)
	}
}

func TestUpdateAssistantConfigForwardsResourceVersion(t *testing.T) {
	f, op, _ := assistantConfigFixture(t)
	op.stored = &v1alpha1.AssistantConfig{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.AssistantConfigName, ResourceVersion: "1"}}
	op.rv = 1
	body := v1alpha1.AssistantConfig{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}}
	rec := f.do(t, f.admin, http.MethodPut, "/assistant/config", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if op.lastPut == nil || op.lastPut.ResourceVersion != "1" {
		t.Fatalf("operator received %+v, want resourceVersion 1", op.lastPut)
	}
	if op.lastPut.Name != v1alpha1.AssistantConfigName {
		t.Fatalf("operator received name %q, want the singleton name", op.lastPut.Name)
	}
}

// TestUpdateAssistantConfigStaleVersionAnswers409 pins that a write with an old
// version is refused, so a second admin's change is not lost.
func TestUpdateAssistantConfigStaleVersionAnswers409(t *testing.T) {
	f, op, _ := assistantConfigFixture(t)
	op.stored = &v1alpha1.AssistantConfig{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.AssistantConfigName, ResourceVersion: "2"}}
	op.rv = 2
	body := v1alpha1.AssistantConfig{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}}
	rec := f.do(t, f.admin, http.MethodPut, "/assistant/config", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale PUT: %d %s, want 409", rec.Code, rec.Body.String())
	}
	var got map[string]any
	decodeBody(t, rec, &got)
	if got["code"] != assistantCfgErrConflict {
		t.Fatalf("stale PUT code %v, want %q", got["code"], assistantCfgErrConflict)
	}
}

func TestUpdateAssistantConfigMapsOperatorCodes(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "invalid object", err: &BroodError{Code: bus.CodeInvalid, Msg: "bad"}, status: http.StatusBadRequest, code: assistantCfgErrInvalid},
		{name: "not found", err: &BroodError{Code: bus.CodeNotFound, Msg: "gone"}, status: http.StatusNotFound, code: assistantCfgErrNotFound},
		{name: "no responder", err: &ErrClusterUnreachable{Cluster: "core", Err: nats.ErrNoResponders}, status: http.StatusServiceUnavailable, code: assistantCfgErrUnavailable},
		{name: "timeout", err: &ErrClusterUnreachable{Cluster: "core", Err: nats.ErrTimeout}, status: http.StatusGatewayTimeout, code: assistantCfgErrTimeout},
		{name: "unknown failure", err: errors.New("boom"), status: http.StatusInternalServerError, code: "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, op, _ := assistantConfigFixture(t)
			op.err = tt.err
			rec := f.do(t, f.admin, http.MethodPut, "/assistant/config", v1alpha1.AssistantConfig{})
			if rec.Code != tt.status {
				t.Fatalf("PUT: %d %s, want %d", rec.Code, rec.Body.String(), tt.status)
			}
			var got map[string]any
			decodeBody(t, rec, &got)
			if got["code"] != tt.code {
				t.Fatalf("code %v, want %q", got["code"], tt.code)
			}
		})
	}
}

func TestPutAssistantConfigPublishesActivity(t *testing.T) {
	f, _, sink := assistantConfigFixture(t)
	rec := f.do(t, f.admin, http.MethodPut, "/assistant/config", v1alpha1.AssistantConfig{})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if len(sink.events) != 1 || sink.events[0].Type != "assistantconfig.updated" {
		t.Fatalf("activity events %+v, want one assistantconfig.updated", sink.events)
	}
	if sink.events[0].Cluster != "core" {
		t.Fatalf("activity cluster %q, want core", sink.events[0].Cluster)
	}
}

var _ activity.EventSink = (*recordingActivitySink)(nil)

func (f *fakeAssistantOperator) SetAssistantKey(_ context.Context, _ string, provider string, key []byte) error {
	if f.keyErr != nil {
		return f.keyErr
	}
	if f.keys == nil {
		f.keys = map[string][]byte{}
	}
	f.keys[provider] = append([]byte(nil), key...)
	return nil
}

func (f *fakeAssistantOperator) ClearAssistantKey(_ context.Context, _ string, provider string) error {
	if f.keyErr != nil {
		return f.keyErr
	}
	f.cleared = append(f.cleared, provider)
	return nil
}
