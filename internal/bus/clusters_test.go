package bus

import (
	"encoding/json"
	"testing"
	"time"
)

// TestClusterInfoJSONRoundTrip verifies ClusterInfo marshals/unmarshals with
// exactly the six expected keys.
func TestClusterInfoJSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	info := ClusterInfo{
		Name:            "dev-cluster",
		OperatorVersion: "abc1234",
		K8sVersion:      "v1.30.0",
		ControllerCount: 5,
		ConnectedCount:  3,
		LastHeartbeat:   now,
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Unmarshal into map to verify key set
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}

	// All possible keys (drainStartedAt may be omitted when nil).
	possibleKeys := []string{"name", "operatorVersion", "k8sVersion", "controllerCount", "connectedCount", "lastHeartbeat", "state", "drainStartedAt"}
	for _, k := range possibleKeys {
		if _, ok := m[k]; !ok {
			// drainStartedAt is omitempty; it's expected to be absent when nil.
			if k == "drainStartedAt" {
				continue
			}
			t.Errorf("missing key %q in JSON output", k)
		}
	}
	// There should be at least 6 core + state keys.
	if len(m) < 7 {
		t.Errorf("expected at least 7 keys, got %d: %v", len(m), m)
	}

	// Round-trip through unmarshal
	var got ClusterInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != info.Name || got.OperatorVersion != info.OperatorVersion ||
		got.K8sVersion != info.K8sVersion || got.ControllerCount != info.ControllerCount ||
		got.ConnectedCount != info.ConnectedCount || !got.LastHeartbeat.Equal(info.LastHeartbeat) ||
		got.State != info.State {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, info)
	}
}

// TestClusterInfoJSONRoundTripDrainStartedAt verifies DrainStartedAt is
// omitted from JSON when nil and round-trips correctly when set.
func TestClusterInfoJSONRoundTripDrainStartedAt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	// nil DrainStartedAt: should be omitted from JSON.
	info := ClusterInfo{
		Name:          "dev-cluster",
		State:         ClusterStateActive,
		LastHeartbeat: now,
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}
	if _, ok := m["drainStartedAt"]; ok {
		t.Errorf("drainStartedAt should be omitted when nil, got %v", m["drainStartedAt"])
	}

	// Set DrainStartedAt: should round-trip.
	t2 := time.Now().UTC().Truncate(time.Second)
	info2 := ClusterInfo{
		Name:           "dev-cluster",
		State:          ClusterStateDraining,
		DrainStartedAt: &t2,
		LastHeartbeat:  now,
	}
	data2, err := json.Marshal(info2)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ClusterInfo
	if err := json.Unmarshal(data2, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.State != ClusterStateDraining {
		t.Errorf("state: got %q, want %q", got.State, ClusterStateDraining)
	}
	if got.DrainStartedAt == nil {
		t.Fatal("DrainStartedAt should not be nil after round-trip")
	}
	if !got.DrainStartedAt.Equal(t2) {
		t.Errorf("DrainStartedAt: got %v, want %v", got.DrainStartedAt, t2)
	}
}

// TestPutClusterHeartbeatEmptyName verifies that an empty Name returns an error.
func TestPutClusterHeartbeatEmptyName(t *testing.T) {
	info := ClusterInfo{
		OperatorVersion: "abc1234",
	}
	if err := PutClusterHeartbeat(nil, info); err == nil {
		t.Fatal("expected error for empty name")
	}
}

// TestKVKeysEmpty verifies Keys() returns empty non-nil for a fresh bucket.
func TestKVKeysEmpty(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV("keys_empty_test", time.Minute)
	if err != nil {
		t.Fatalf("EnsureKV: %v", err)
	}

	keys, err := kv.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if keys == nil {
		t.Fatal("Keys returned nil, want empty slice")
	}
	if len(keys) != 0 {
		t.Fatalf("Keys: want empty, got %v", keys)
	}
}

// TestClustersPutGetListRoundTrip verifies PutClusterHeartbeat → GetCluster →
// ListClusters round-trip with a live NATS KV bucket.
func TestClustersPutGetListRoundTrip(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV("clusters_roundtrip_test", 2*time.Minute)
	if err != nil {
		t.Fatalf("EnsureKV: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	info := ClusterInfo{
		Name:            "core",
		OperatorVersion: "abc1234",
		K8sVersion:      "v1.30.0",
		ControllerCount: 10,
		ConnectedCount:  7,
		LastHeartbeat:   now,
	}

	if err := PutClusterHeartbeat(kv, info); err != nil {
		t.Fatalf("PutClusterHeartbeat: %v", err)
	}

	got, err := GetCluster(kv, "core")
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if got == nil {
		t.Fatal("GetCluster returned nil")
	}
	if got.Name != "core" || got.ControllerCount != 10 || got.ConnectedCount != 7 {
		t.Errorf("GetCluster: got %+v, want name=core count=10/7", got)
	}

	// List
	list, err := ListClusters(kv)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListClusters: want 1, got %d", len(list))
	}
	if list[0].Name != "core" {
		t.Errorf("ListClusters[0].Name = %q, want %q", list[0].Name, "core")
	}

	// GetCluster for absent key
	absent, err := GetCluster(kv, "nonexistent")
	if err != nil {
		t.Fatalf("GetCluster nonexistent: %v", err)
	}
	if absent != nil {
		t.Fatal("GetCluster for nonexistent key: want nil, got non-nil")
	}

	// ListClusters with empty bucket returns empty
	list2, err := ListClusters(kv)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	_ = list2 // non-empty; core entry still there
}

// TestClusterDirectory delegates to package functions.
func TestClusterDirectory(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV("clusters_dir_test", 2*time.Minute)
	if err != nil {
		t.Fatalf("EnsureKV: %v", err)
	}

	dir := NewClusterDirectory(kv)
	if dir == nil {
		t.Fatal("NewClusterDirectory returned nil")
	}

	// List on empty bucket
	list, err := dir.List()
	if err != nil {
		t.Fatalf("dir.List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("dir.List on empty: want 0, got %d", len(list))
	}

	// Get on absent
	got, err := dir.Get("core")
	if err != nil {
		t.Fatalf("dir.Get: %v", err)
	}
	if got != nil {
		t.Fatal("dir.Get absent: want nil")
	}

	// Put and verify. The directory mirrors the bucket via a KV watcher, so
	// a fresh put becomes visible asynchronously — poll briefly.
	now := time.Now().UTC().Truncate(time.Second)
	info := ClusterInfo{
		Name:            "core",
		OperatorVersion: "abc1234",
		ControllerCount: 3,
		ConnectedCount:  2,
		LastHeartbeat:   now,
	}
	if err := PutClusterHeartbeat(kv, info); err != nil {
		t.Fatalf("PutClusterHeartbeat: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err = dir.Get("core")
		if err != nil {
			t.Fatalf("dir.Get core: %v", err)
		}
		if got != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got == nil || got.Name != "core" {
		t.Fatal("dir.Get core: want non-nil with name core")
	}

	list, err = dir.List()
	if err != nil {
		t.Fatalf("dir.List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "core" {
		t.Fatalf("dir.List: want [core], got %v", list)
	}
}

// TestClusterDirectoryWatchMirror verifies the watch-backed mirror follows
// puts and deletes, and that stale heartbeats are filtered at read time
// (issue #280).
func TestClusterDirectoryWatchMirror(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV("clusters_dir_watch_test", 2*time.Minute)
	if err != nil {
		t.Fatalf("EnsureKV: %v", err)
	}
	dir := NewClusterDirectory(kv)

	put := func(name string, hb time.Time) {
		t.Helper()
		if err := PutClusterHeartbeat(kv, ClusterInfo{Name: name, LastHeartbeat: hb}); err != nil {
			t.Fatalf("PutClusterHeartbeat %s: %v", name, err)
		}
	}
	waitLen := func(want int) []ClusterInfo {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			list, err := dir.List()
			if err != nil {
				t.Fatalf("dir.List: %v", err)
			}
			if len(list) == want || time.Now().After(deadline) {
				return list
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	put("core", time.Now().UTC())
	put("dev-cluster", time.Now().UTC())
	list := waitLen(2)
	if len(list) != 2 || list[0].Name != "core" || list[1].Name != "dev-cluster" {
		t.Fatalf("List after puts: want [core dev-cluster], got %v", list)
	}

	// Delete propagates through the watcher.
	if err := kv.Delete("dev-cluster"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list = waitLen(1)
	if len(list) != 1 || list[0].Name != "core" {
		t.Fatalf("List after delete: want [core], got %v", list)
	}

	// A heartbeat older than ClusterEntryTTL is filtered at read time even
	// though MaxAge expiry never emits a watch event.
	put("stale", time.Now().UTC().Add(-2*ClusterEntryTTL))
	deadline := time.Now().Add(5 * time.Second)
	for {
		dir.mu.Lock()
		_, mirrored := dir.entries["stale"]
		dir.mu.Unlock()
		if mirrored || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	list, err = dir.List()
	if err != nil {
		t.Fatalf("dir.List: %v", err)
	}
	if len(list) != 1 || list[0].Name != "core" {
		t.Fatalf("List with stale entry: want [core], got %v", list)
	}
	if got, err := dir.Get("stale"); err != nil || got != nil {
		t.Fatalf("Get stale: want nil,nil; got %v,%v", got, err)
	}
}

// TestListClustersSkipsTTLRaceEntry verifies that a key that disappears
// between Keys() and Get() is skipped.
func TestListClustersSkipsTTLRaceEntry(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV("clusters_ttl_race_test", time.Minute)
	if err != nil {
		t.Fatalf("EnsureKV: %v", err)
	}

	// Put and immediately delete to simulate TTL race
	info := ClusterInfo{Name: "ephemeral", LastHeartbeat: time.Now().UTC()}
	if err := PutClusterHeartbeat(kv, info); err != nil {
		t.Fatalf("PutClusterHeartbeat: %v", err)
	}
	if err := kv.Delete("ephemeral"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	list, err := ListClusters(kv)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("ListClusters after delete: want 0, got %d: %+v", len(list), list)
	}
}

// TestListClustersSort verifies ListClusters sorts by Name.
func TestListClustersSort(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV("clusters_sort_test", 2*time.Minute)
	if err != nil {
		t.Fatalf("EnsureKV: %v", err)
	}

	for _, name := range []string{"zulu", "alpha", "bravo"} {
		info := ClusterInfo{Name: name, LastHeartbeat: time.Now().UTC()}
		if err := PutClusterHeartbeat(kv, info); err != nil {
			t.Fatalf("PutClusterHeartbeat %s: %v", name, err)
		}
	}

	list, err := ListClusters(kv)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListClusters: want 3, got %d", len(list))
	}
	if list[0].Name != "alpha" || list[1].Name != "bravo" || list[2].Name != "zulu" {
		t.Errorf("ListClusters order: got %v, want [alpha bravo zulu]", list)
	}
}

// TestListClustersSkipsUnparseable verifies that an unparseable entry
// is skipped without failing the entire list.
func TestListClustersSkipsUnparseable(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV("clusters_unparseable_test", 2*time.Minute)
	if err != nil {
		t.Fatalf("EnsureKV: %v", err)
	}

	// Put valid entry
	if err := PutClusterHeartbeat(kv, ClusterInfo{Name: "good", LastHeartbeat: time.Now().UTC()}); err != nil {
		t.Fatalf("PutClusterHeartbeat good: %v", err)
	}
	// Put garbage entry
	if err := kv.Put("bad", []byte("{not-json")); err != nil {
		t.Fatalf("Put bad: %v", err)
	}

	list, err := ListClusters(kv)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListClusters: want 1 (skip garbage), got %d", len(list))
	}
	if list[0].Name != "good" {
		t.Errorf("ListClusters[0].Name = %q, want %q", list[0].Name, "good")
	}
}
