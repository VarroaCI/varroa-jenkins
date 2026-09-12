package controller

import (
	"log/slog"
	"reflect"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
)

// clearCellsEnv unsets every env var ParseCellsConfigFromEnv reads, so each
// test starts from a clean slate regardless of the OS environment.
func clearCellsEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"VARROA_CELLS_WARM_POOL_ENABLED",
		"VARROA_CELLS_WARM_POOL_PER_PROFILE",
		"VARROA_CELLS_MAX_TENANTS",
		"VARROA_CELLS_UNREADY_GRACE",
		"VARROA_NETWORKPOLICY_ENABLED",
		"VARROA_NETWORKPOLICY_APISERVER_EGRESS",
		"VARROA_CELL_ISOLATION",
	} {
		t.Setenv(key, "")
	}
}

func TestParseCellsConfigFromEnv_Defaults(t *testing.T) {
	clearCellsEnv(t)

	got := ParseCellsConfigFromEnv(slog.Default())

	want := CellsConfig{
		WarmPoolEnabled:    false,
		WarmPoolPerProfile: 1,
		MaxTenantsPerCell:  10,
		UnreadyGracePeriod: 5 * time.Minute,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseCellsConfigFromEnv() defaults = %+v, want %+v", got, want)
	}
}

func TestParseCellsConfigFromEnv_JSONFields(t *testing.T) {
	clearCellsEnv(t)

	t.Setenv("VARROA_CELLS_WARM_POOL_ENABLED", "true")
	t.Setenv("VARROA_CELLS_WARM_POOL_PER_PROFILE", "3")
	t.Setenv("VARROA_CELLS_MAX_TENANTS", "25")
	t.Setenv("VARROA_CELLS_UNREADY_GRACE", "10m")
	t.Setenv("VARROA_NETWORKPOLICY_ENABLED", "true")
	t.Setenv("VARROA_NETWORKPOLICY_APISERVER_EGRESS", `{"cidrs":["10.0.0.0/8"],"ports":[443,6443]}`)
	t.Setenv("VARROA_CELL_ISOLATION", `{"enabled":true,"extraIngress":[{"ports":[{"port":8080}]}],"extraEgress":[]}`)

	got := ParseCellsConfigFromEnv(slog.Default())

	if !got.WarmPoolEnabled {
		t.Error("WarmPoolEnabled = false, want true")
	}
	if got.WarmPoolPerProfile != 3 {
		t.Errorf("WarmPoolPerProfile = %d, want 3", got.WarmPoolPerProfile)
	}
	if got.MaxTenantsPerCell != 25 {
		t.Errorf("MaxTenantsPerCell = %d, want 25", got.MaxTenantsPerCell)
	}
	if got.UnreadyGracePeriod != 10*time.Minute {
		t.Errorf("UnreadyGracePeriod = %v, want 10m", got.UnreadyGracePeriod)
	}
	if !got.NetworkPolicyEnabled {
		t.Error("NetworkPolicyEnabled = false, want true")
	}

	wantEgress := APIServerEgress{CIDRs: []string{"10.0.0.0/8"}, Ports: []int32{443, 6443}}
	if got.APIServerEgress.CIDRs[0] != wantEgress.CIDRs[0] || len(got.APIServerEgress.Ports) != 2 {
		t.Errorf("APIServerEgress = %+v, want %+v", got.APIServerEgress, wantEgress)
	}

	if !got.CellIsolation.Enabled {
		t.Error("CellIsolation.Enabled = false, want true")
	}
	if len(got.CellIsolation.ExtraIngress) != 1 {
		t.Fatalf("CellIsolation.ExtraIngress = %+v, want 1 entry", got.CellIsolation.ExtraIngress)
	}
	wantPort := networkingv1.NetworkPolicyPort{Port: got.CellIsolation.ExtraIngress[0].Ports[0].Port}
	if wantPort.Port == nil || wantPort.Port.IntValue() != 8080 {
		t.Errorf("CellIsolation.ExtraIngress[0].Ports[0].Port = %v, want 8080", got.CellIsolation.ExtraIngress[0].Ports[0].Port)
	}
	if len(got.CellIsolation.ExtraEgress) != 0 {
		t.Errorf("CellIsolation.ExtraEgress = %+v, want empty", got.CellIsolation.ExtraEgress)
	}
}

func TestParseCellsConfigFromEnv_BadDuration(t *testing.T) {
	clearCellsEnv(t)

	t.Setenv("VARROA_CELLS_UNREADY_GRACE", "not-a-duration")

	got := ParseCellsConfigFromEnv(slog.Default())

	if got.UnreadyGracePeriod != 5*time.Minute {
		t.Errorf("UnreadyGracePeriod = %v, want fallback 5m on parse error", got.UnreadyGracePeriod)
	}
}

func TestParseCellsConfigFromEnv_BadJSON(t *testing.T) {
	clearCellsEnv(t)

	t.Setenv("VARROA_CELL_ISOLATION", "{not valid json")

	got := ParseCellsConfigFromEnv(slog.Default())

	if got.CellIsolation.Enabled {
		t.Error("CellIsolation.Enabled = true, want fallback zero value on parse error")
	}
}

func TestParseCellsConfigFromEnv_PerProfileFloorIsOne(t *testing.T) {
	t.Setenv("VARROA_CELLS_WARM_POOL_PER_PROFILE", "0")
	got := ParseCellsConfigFromEnv(slog.Default())
	if got.WarmPoolPerProfile != 1 {
		t.Errorf("WarmPoolPerProfile = %d, want 1", got.WarmPoolPerProfile)
	}
}
