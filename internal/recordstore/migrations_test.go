package recordstore

import "testing"

// TestMigrationsFSListsEveryFile asserts the embedded filesystem ships
// every migration file the package needs at runtime.
func TestMigrationsFSListsEveryFile(t *testing.T) {
	entries, err := migrationsFS.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}

	want := map[string]bool{
		"00001_initial_schema.sql":               false,
		"00002_activity.sql":                     false,
		"00003_job_schedules.sql":                false,
		"00004_search_paths.sql":                 false,
		"00005_tenant_placements.sql":            false,
		"00006_fleet_locks.sql":                  false,
		"00007_job_schedule_upstream.sql":        false,
		"00008_trigger_failed_result.sql":        false,
		"00009_run_intents_refired_at.sql":       false,
		"00010_run_state_object_class.sql":       false,
		"00011_tenant_members.sql":               false,
		"00012_runs_heartbeat_received_at.sql":   false,
		"00013_tenant_placement_incarnation.sql": false,
		"00014_runs_source.sql":                  false,
		"00015_runs_summary.sql":                 false,
		"00016_runs_inputs.sql":                  false,
	}
	for _, entry := range entries {
		if _, ok := want[entry.Name()]; !ok {
			t.Fatalf("embedded filesystem lists unexpected file %q", entry.Name())
		}
		want[entry.Name()] = true
	}
	for name, found := range want {
		if !found {
			t.Fatalf("embedded filesystem is missing %q", name)
		}
	}
}
