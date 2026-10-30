package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestNATSSyncAnnouncer_BadgeTokenOutsidePolicy asserts the badge token rides
// the announce on its own field and never appears in the policy JSON, so the
// record store cannot log it as a policy setting.
func TestNATSSyncAnnouncer_BadgeTokenOutsidePolicy(t *testing.T) {
	conn := startTestSyncBus(t)

	var raw []byte
	var got recordstore.TenantSyncRequest
	sub, err := conn.SubscribeRequest(bus.TenantSyncSubject, "recorder", func(data []byte) []byte {
		raw = append([]byte(nil), data...)
		_ = json.Unmarshal(data, &got)
		resp, _ := json.Marshal(recordstore.TenantSyncResponse{Ok: true})
		return resp
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	const token = "tok-badge-secret-0123456789"
	_, err = NewNATSSyncAnnouncer(conn)(context.Background(), cells.AnnounceRequest{
		Cluster: "core", Tenant: "payments", Cell: "cell-a", Generation: 3, CellInstance: "uid-1",
		Policy: cells.SyncPolicy{BadgeToken: token},
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if got.BadgeToken != token {
		t.Errorf("announce BadgeToken = %q, want the token", got.BadgeToken)
	}
	if n := strings.Count(string(raw), token); n != 1 {
		t.Errorf("token appears %d times in the request, want once (the badge_token field only)", n)
	}
	var doc struct {
		Policy json.RawMessage `json:"policy"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if strings.Contains(string(doc.Policy), token) || strings.Contains(string(doc.Policy), "badge") {
		t.Errorf("policy carries the badge token: %s", doc.Policy)
	}
}
