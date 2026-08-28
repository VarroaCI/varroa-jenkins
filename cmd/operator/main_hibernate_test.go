package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
)

type fakeHibernationRunner struct {
	hibernateErr error
	wakeErr      error
	hibernated   []string // "ns/name"
	woken        []string // "ns/name"
}

func (f *fakeHibernationRunner) HibernateController(_ context.Context, namespace, name string) error {
	f.hibernated = append(f.hibernated, namespace+"/"+name)
	return f.hibernateErr
}

func (f *fakeHibernationRunner) WakeControllerAction(_ context.Context, namespace, name string) error {
	f.woken = append(f.woken, namespace+"/"+name)
	return f.wakeErr
}

func TestHandleHibernate_NotFoundMapsToCodeNotFound(t *testing.T) {
	runner := &fakeHibernationRunner{
		hibernateErr: apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "controllers"}, "ghost"),
	}
	resp := handleHibernate(context.Background(), slog.Default(), runner, mustJSON(t, bus.HibernateRequest{Namespace: "team-a", Name: "ghost"}))

	var out bus.HibernateResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if out.Code != bus.CodeNotFound {
		t.Fatalf("code = %q, want %q (error %q)", out.Code, bus.CodeNotFound, out.Error)
	}
}

func TestHandleWake_NotFoundMapsToCodeNotFound(t *testing.T) {
	runner := &fakeHibernationRunner{
		wakeErr: apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "controllers"}, "ghost"),
	}
	resp := handleWake(context.Background(), slog.Default(), runner, mustJSON(t, bus.WakeRequest{Namespace: "team-a", Name: "ghost"}))

	var out bus.WakeResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if out.Code != bus.CodeNotFound {
		t.Fatalf("code = %q, want %q (error %q)", out.Code, bus.CodeNotFound, out.Error)
	}
}

func TestHandleHibernate_StoppedMapsToCodeConflict(t *testing.T) {
	runner := &fakeHibernationRunner{hibernateErr: controller.ErrControllerStopped}
	resp := handleHibernate(context.Background(), slog.Default(), runner, mustJSON(t, bus.HibernateRequest{Namespace: "team-a", Name: "ctrl"}))

	var out bus.HibernateResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if out.Code != bus.CodeConflict {
		t.Fatalf("code = %q, want %q (error %q)", out.Code, bus.CodeConflict, out.Error)
	}
}

func TestHandleWake_OtherErrorMapsToCodeInternal(t *testing.T) {
	runner := &fakeHibernationRunner{wakeErr: errors.New("boom")}
	resp := handleWake(context.Background(), slog.Default(), runner, mustJSON(t, bus.WakeRequest{Namespace: "team-a", Name: "ctrl"}))

	var out bus.WakeResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if out.Code != bus.CodeInternal {
		t.Fatalf("code = %q, want %q (error %q)", out.Code, bus.CodeInternal, out.Error)
	}
}

func TestHandleHibernate_Success(t *testing.T) {
	runner := &fakeHibernationRunner{}
	resp := handleHibernate(context.Background(), slog.Default(), runner, mustJSON(t, bus.HibernateRequest{Namespace: "team-a", Name: "ctrl"}))

	var out bus.HibernateResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if out.Code != "" || out.Error != "" {
		t.Fatalf("expected success, got code=%q error=%q", out.Code, out.Error)
	}
	if len(runner.hibernated) != 1 || runner.hibernated[0] != "team-a/ctrl" {
		t.Fatalf("hibernated calls = %v, want exactly [team-a/ctrl]", runner.hibernated)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
