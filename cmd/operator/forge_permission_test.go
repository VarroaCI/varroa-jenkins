package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// permissionOps records the permission request and answers write.
type permissionOps struct {
	stampOps
	got *forgeapi.PermissionRequest
}

func (p permissionOps) Permission(_ context.Context, req forgeapi.PermissionRequest) forgeapi.PermissionReply {
	*p.got = req
	return forgeapi.PermissionReply{Status: forgeapi.Status{OK: true}, Permission: "write"}
}

func ownedEnter(_ context.Context, _ string) (context.Context, func(), bool) {
	return context.Background(), func() {}, true
}

func notOwnedEnter(_ context.Context, _ string) (context.Context, func(), bool) {
	return nil, nil, false
}

func TestHandleForgePermission_AnswersOwner(t *testing.T) {
	var got forgeapi.PermissionRequest
	ops := permissionOps{got: &got}
	data, err := json.Marshal(forgeapi.PermissionRequest{Tenant: "payments", Repo: "https://github.com/acme/app", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	reply := handleForgePermission(context.Background(), slog.Default(), ops, ownedEnter, data)
	var out forgeapi.PermissionReply
	if err := json.Unmarshal(reply, &out); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if !out.OK || out.Permission != "write" {
		t.Fatalf("reply %+v", out)
	}
	if got.Login != "alice" || got.Tenant != "payments" {
		t.Fatalf("request reached the ops as %+v", got)
	}
}

func TestHandleForgePermission_NonOwnerSendsNoReply(t *testing.T) {
	var got forgeapi.PermissionRequest
	data, _ := json.Marshal(forgeapi.PermissionRequest{Tenant: "payments", Login: "alice"})
	if reply := handleForgePermission(context.Background(), slog.Default(), permissionOps{got: &got}, notOwnedEnter, data); reply != nil {
		t.Fatalf("a non-owner replied: %s", reply)
	}
}

func TestHandleForgePermission_DecodeFailure(t *testing.T) {
	var got forgeapi.PermissionRequest
	reply := handleForgePermission(context.Background(), slog.Default(), permissionOps{got: &got}, ownedEnter, []byte("{"))
	var out forgeapi.PermissionReply
	if err := json.Unmarshal(reply, &out); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if out.OK || out.Code != forgeapi.CodeInvalidRequest {
		t.Fatalf("reply %+v, want invalid_request", out)
	}
}
