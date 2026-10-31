package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// unreachableRecordReader answers ErrUnavailable from every call it serves.
type unreachableRecordReader struct {
	recordstore.Reader
}

func (unreachableRecordReader) GetRun(context.Context, string) (recordstore.Run, []recordstore.Stage, error) {
	return recordstore.Run{}, nil, fmt.Errorf("%w: read run: connection refused", recordstore.ErrUnavailable)
}

func (unreachableRecordReader) ListAllTenantPlacements(context.Context) ([]recordstore.TenantPlacement, error) {
	return nil, fmt.Errorf("%w: list placements", recordstore.ErrUnavailable)
}

// unreachableTenantResolver answers ErrUnavailable from Resolve, as a
// placement read does when the record store does not answer.
type unreachableTenantResolver struct{}

func (unreachableTenantResolver) Resolve(context.Context, *auth.Claims, string) ([]TenantRef, error) {
	return nil, fmt.Errorf("read placement: %w", recordstore.ErrUnavailable)
}

func (unreachableTenantResolver) ResolveRun(context.Context, *auth.Claims, recordstore.Run) (TenantRef, error) {
	return TenantRef{}, recordstore.ErrUnavailable
}

func newUnreachableTestServer(resolver TenantResolver) *Server {
	return NewServer(&Dependencies{
		RecordStore:    unreachableRecordReader{},
		TenantResolver: resolver,
		Logger:         slog.Default(),
	})
}

func TestHandleReaderErrorUnreachable(t *testing.T) {
	srv := newUnreachableTestServer(nil)
	rec := httptest.NewRecorder()
	err := fmt.Errorf("read: %w", recordstore.ErrUnavailable)
	if !srv.handleReaderError(rec, err, "list runs") {
		t.Fatal("handleReaderError returned false, want true for an error")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["code"] != codeRecordStoreUnreachable {
		t.Fatalf("code = %q, want %q (body %s)", body["code"], codeRecordStoreUnreachable, rec.Body.String())
	}
}

func TestReaderServiceErrorUnreachable(t *testing.T) {
	srv := newUnreachableTestServer(nil)
	serr := srv.readerServiceError(fmt.Errorf("read: %w", recordstore.ErrUnavailable), "list runs")
	if serr == nil {
		t.Fatal("readerServiceError returned nil")
	}
	if serr.Status != http.StatusServiceUnavailable || serr.Code != codeRecordStoreUnreachable {
		t.Fatalf("ServiceError = %+v, want 503 with code %q", serr, codeRecordStoreUnreachable)
	}
}

func TestResolveTenantUnreachable503(t *testing.T) {
	srv := newUnreachableTestServer(unreachableTenantResolver{})
	refs, serr := srv.resolveTenantTenants(context.Background(), nil, "tenant-a")
	if refs != nil {
		t.Fatalf("refs = %v, want nil", refs)
	}
	if serr == nil || serr.Status != http.StatusServiceUnavailable || serr.Code != codeRecordStoreUnreachable {
		t.Fatalf("ServiceError = %+v, want 503 with code %q", serr, codeRecordStoreUnreachable)
	}
	rec := httptest.NewRecorder()
	srv.writeServiceError(rec, serr)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["code"] != codeRecordStoreUnreachable {
		t.Fatalf("code = %q, want %q", body["code"], codeRecordStoreUnreachable)
	}
}

func TestAllTenantPlacementsUnreachableDegrades(t *testing.T) {
	srv := newUnreachableTestServer(nil)
	read, err := srv.allTenantPlacements(context.Background())
	if err != nil {
		t.Fatalf("allTenantPlacements error = %v, want nil (a degraded read, not a failure)", err)
	}
	if read.Rows != nil {
		t.Fatalf("allTenantPlacements rows = %v, want none", read.Rows)
	}
	if !errors.Is(read.Degraded, recordstore.ErrUnavailable) {
		t.Fatalf("allTenantPlacements Degraded = %v, want ErrUnavailable", read.Degraded)
	}
}
