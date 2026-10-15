package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// pagedRequest answers a list request from pages keyed by the After value.
// It records every After value it receives, in order.
func pagedRequest(t *testing.T, pages map[string]any) (func(string, []byte, time.Duration) ([]byte, error), *[]string) {
	t.Helper()
	var afters []string
	return func(_ string, data []byte, _ time.Duration) ([]byte, error) {
		var req bus.ConfigListRequest
		if err := json.Unmarshal(data, &req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		afters = append(afters, req.After)
		page, ok := pages[req.After]
		if !ok {
			t.Fatalf("unexpected page after %q", req.After)
		}
		return json.Marshal(page)
	}, &afters
}

func pagedBrood(request func(string, []byte, time.Duration) ([]byte, error)) *busConfigBrood {
	return &busConfigBrood{localCluster: "core", logger: slog.Default(), request: request}
}

func TestConfigBrood_ListCancelStopsPaging(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	secondStarted := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var calls atomic.Int32
	f := pagedBrood(func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		if calls.Add(1) == 1 {
			return json.Marshal(bus.ConfigListResponse{
				Items: []json.RawMessage{json.RawMessage(`{"name":"a"}`)},
				Next:  "a",
			})
		}
		close(secondStarted)
		<-release
		return nil, errors.New("released")
	})
	type result struct {
		items []json.RawMessage
		err   error
	}
	done := make(chan result, 1)
	go func() {
		items, err := f.ListJenkinsRoles(ctx, "member-b")
		done <- result{items, err}
	}()
	<-secondStarted
	cancel()
	select {
	case got := <-done:
		var be *BroodError
		if !errors.As(got.err, &be) || be.Msg != "list cancelled" {
			t.Fatalf("err = %v, want list cancelled", got.err)
		}
		if got.items != nil {
			t.Fatalf("items = %v, want nil", got.items)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("list did not return after cancel")
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestConfigBrood_ListCatalogItemsReadsEveryPage(t *testing.T) {
	request, afters := pagedRequest(t, map[string]any{
		"": bus.ConfigListResponse{
			Items: []json.RawMessage{json.RawMessage(`{"name":"a"}`)},
			Next:  "ns-a/a",
		},
		"ns-a/a": bus.ConfigListResponse{
			Items: []json.RawMessage{json.RawMessage(`{"name":"b"}`)},
		},
	})
	f := pagedBrood(request)
	items, err := f.ListCatalogItems(context.Background(), "member-b", "ns-a", CatalogItemFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if len(*afters) != 2 || (*afters)[0] != "" || (*afters)[1] != "ns-a/a" {
		t.Fatalf("after values = %v, want [\"\" \"ns-a/a\"]", *afters)
	}
}

func TestConfigBrood_ListFailsWholeOnPageError(t *testing.T) {
	request, _ := pagedRequest(t, map[string]any{
		"": bus.ConfigListResponse{
			Items: []json.RawMessage{json.RawMessage(`{"name":"a"}`)},
			Next:  "a",
		},
		"a": bus.ConfigListResponse{Code: bus.CodeInternal, Error: "boom"},
	})
	f := pagedBrood(request)
	items, err := f.ListJenkinsRoles(context.Background(), "member-b")
	var be *BroodError
	if !errors.As(err, &be) || be.Code != bus.CodeInternal || be.Msg != "boom" {
		t.Fatalf("err = %v, want a BroodError internal boom", err)
	}
	if items != nil {
		t.Fatalf("items = %v, want none after a failed page", items)
	}
}

func TestConfigBrood_ViewVersionProfilesPages(t *testing.T) {
	viewA, err := json.Marshal(bus.VersionProfileView{Item: json.RawMessage(`{"metadata":{"name":"a"}}`), ResolvedPlugins: []string{"git@5.2"}})
	if err != nil {
		t.Fatal(err)
	}
	viewB, err := json.Marshal(bus.VersionProfileView{Item: json.RawMessage(`{"metadata":{"name":"b"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	request, afters := pagedRequest(t, map[string]any{
		"":  bus.VersionProfileViewResponse{Profiles: []json.RawMessage{viewA}, Next: "a"},
		"a": bus.VersionProfileViewResponse{Profiles: []json.RawMessage{viewB}},
	})
	f := pagedBrood(request)
	views, err := f.ViewVersionProfiles(context.Background(), "member-b")
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("views = %d, want 2", len(views))
	}
	if len(views[0].ResolvedPlugins) != 1 || views[0].ResolvedPlugins[0] != "git@5.2" {
		t.Fatalf("resolved plugins of first view = %v", views[0].ResolvedPlugins)
	}
	if len(*afters) != 2 {
		t.Fatalf("requests = %d, want 2", len(*afters))
	}
}

func TestConfigBrood_ListCellPoolsPages(t *testing.T) {
	request, afters := pagedRequest(t, map[string]any{
		"":     bus.ConfigListResponse{Items: []json.RawMessage{rawPool(t, "p-a", 1)}, Next: "/p-a"},
		"/p-a": bus.ConfigListResponse{Items: []json.RawMessage{rawPool(t, "p-b", 1)}},
	})
	f := pagedBrood(request)
	pools, err := f.ListCellPools(context.Background(), "member-b")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pools) != 2 || pools[0].Name != "p-a" || pools[1].Name != "p-b" {
		t.Fatalf("pools = %+v, want p-a and p-b", pools)
	}
	if len(*afters) != 2 {
		t.Fatalf("requests = %d, want 2", len(*afters))
	}
}
