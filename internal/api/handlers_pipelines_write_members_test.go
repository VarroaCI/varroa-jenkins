package api

import (
	"errors"
	"net/http"
	"testing"
)

// TestHandleCreateRun_PublishesToTheJobsHomeMember asserts the trigger
// write resolves the job's home member through the record store before it
// publishes. A job homed on slot 1 reaches slot 1's holder, and not slot
// 0's, which the TenantRef alone names.
func TestHandleCreateRun_PublishesToTheJobsHomeMember(t *testing.T) {
	reader := newFakeRecordReader()
	// The write harness resolves a controller tenant, whose name is
	// "<namespace>/<controller>".
	reader.jobHomes["core/test-ns/ci/svc/main"] = jobHomeAnswer{namespace: "cells-ns", cell: "cell-b"}
	mt := &fakeTriggerTransport{}
	srv := writeTestServer(reader, mt)

	rec := postCreateRun(t, srv, createRunRequest{})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	if calls[0].name != "cell-b" || calls[0].ns != "cells-ns" {
		t.Fatalf("published to %s/%s, want cells-ns/cell-b", calls[0].ns, calls[0].name)
	}
}

// TestHandleCreateRun_KeepsTheTenantRefWhenNoPlacementRowExists asserts a
// tenant with no placement row keeps today's coordinate. That is a
// controller with no cell role, which holds one Jenkins and no member
// list.
func TestHandleCreateRun_KeepsTheTenantRefWhenNoPlacementRowExists(t *testing.T) {
	mt := &fakeTriggerTransport{}
	srv := writeTestServer(newFakeRecordReader(), mt)

	rec := postCreateRun(t, srv, createRunRequest{})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}
	calls := mt.calls()
	if len(calls) != 1 {
		t.Fatalf("published %d commands, want 1", len(calls))
	}
	if calls[0].name != "ci" || calls[0].ns != "test-ns" {
		t.Fatalf("published to %s/%s, want test-ns/ci", calls[0].ns, calls[0].name)
	}
}

// TestHandleCreateRun_HoldsAnUnboundHomeSlot asserts a placed tenant whose
// home slot holds no cell answers a retryable 503 and publishes nothing.
//
// Publishing to slot 0 instead would answer the permanent NOT_HOME at the
// claim fence, and the plugin would cancel a build this API already
// reported accepted.
func TestHandleCreateRun_HoldsAnUnboundHomeSlot(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobHomes["core/test-ns/ci/svc/main"] = jobHomeAnswer{}
	mt := &fakeTriggerTransport{}
	srv := writeTestServer(reader, mt)

	rec := postCreateRun(t, srv, createRunRequest{})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s, want 503", rec.Code, rec.Body.String())
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("published %d commands, want none", len(mt.calls()))
	}
}

// TestHandleCreateRun_HoldsAFailedHomeLookup asserts a failed read answers
// a retryable 503 and publishes nothing, rather than guessing slot 0.
func TestHandleCreateRun_HoldsAFailedHomeLookup(t *testing.T) {
	reader := newFakeRecordReader()
	reader.jobHomeErr = errors.New("read failed")
	mt := &fakeTriggerTransport{}
	srv := writeTestServer(reader, mt)

	rec := postCreateRun(t, srv, createRunRequest{})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s, want 503", rec.Code, rec.Body.String())
	}
	if len(mt.calls()) != 0 {
		t.Fatalf("published %d commands, want none", len(mt.calls()))
	}
}
