package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/assistantconfig"
)

// TestAssistantRequestsFollowNewSnapshot proves that each request reads the
// snapshot current at its start, so a stored change reaches the next request
// with no restart. The day budget in the view comes from the snapshot.
func TestAssistantRequestsFollowNewSnapshot(t *testing.T) {
	f := newAssistantFixture(t)
	first := dayBudgetOf(t, f)
	if first != 1000000 {
		t.Fatalf("day budget before the change = %d, want 1000000", first)
	}
	reg, err := provider.NewRegistry(provider.Config{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	f.rt.Live.Store(&assistantconfig.Snapshot{
		Config:   provider.Config{Budgets: provider.Budgets{UserDay: 777, Session: 100000, TenantDay: 1000000, Steps: 32}},
		Registry: reg,
	})
	if got := dayBudgetOf(t, f); got != 777 {
		t.Fatalf("day budget after the change = %d, want 777", got)
	}
}

// dayBudgetOf reads the dayBudget of the viewer's own assistant view.
func dayBudgetOf(t *testing.T, f *assistantFixture) int64 {
	t.Helper()
	rec := f.do(t, f.viewer, http.MethodGet, "/me/assistant", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/assistant = %d body = %s", rec.Code, rec.Body)
	}
	var body struct {
		DayBudget int64 `json:"dayBudget"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.DayBudget
}

// TestMyAssistantDayBudgetFromSnapshot proves that the day budget of the view is
// the user day budget of the snapshot the request reads.
func TestMyAssistantDayBudgetFromSnapshot(t *testing.T) {
	f := newAssistantFixture(t)
	reg, err := provider.NewRegistry(provider.Config{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	f.rt.Live.Store(&assistantconfig.Snapshot{
		Config:   provider.Config{Budgets: provider.Budgets{UserDay: 555}},
		Registry: reg,
	})
	if got := dayBudgetOf(t, f); got != 555 {
		t.Fatalf("day budget = %d, want 555 from the snapshot", got)
	}
}

// TestGetAssistantSettingsHasNoLimits proves that the settings view no longer
// carries the providers, the defaults, or the limits. Those come from the
// AssistantConfig routes.
func TestGetAssistantSettingsHasNoLimits(t *testing.T) {
	f := newAssistantFixture(t)
	rec := f.do(t, f.admin, http.MethodGet, "/assistant/settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings = %d body = %s", rec.Code, rec.Body)
	}
	for _, key := range []string{"limits", "defaultProvider", "defaultModel", "egress"} {
		if strings.Contains(rec.Body.String(), `"`+key+`"`) {
			t.Errorf("settings carry %q: %s", key, rec.Body)
		}
	}
}
