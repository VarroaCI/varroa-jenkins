package controller

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// pageRow is a metav1.Object test row with a padded body.
type pageRow struct {
	metav1.ObjectMeta
	Pad string `json:"pad"`
}

func pageRowOf(namespace, name string, padBytes int) *pageRow {
	return &pageRow{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Pad:        strings.Repeat("x", padBytes),
	}
}

func marshalPageRow(r *pageRow) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"key": listPageKey(r), "pad": r.Pad})
}

// walkPages reads every page of rows with the given budget and returns the
// keys in the order they arrived.
func walkPages(t *testing.T, rows []*pageRow, budget int) []string {
	t.Helper()
	var keys []string
	after := ""
	for i := 0; i < 1000; i++ {
		page, next, err := pageList(rows, after, budget, marshalPageRow)
		if err != nil {
			t.Fatalf("page after %q: %v", after, err)
		}
		if len(page) == 0 && next != "" {
			t.Fatalf("empty page with next %q", next)
		}
		for _, raw := range page {
			var row struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatalf("decode row: %v", err)
			}
			keys = append(keys, row.Key)
		}
		if next == "" {
			return keys
		}
		after = next
	}
	t.Fatal("paging did not end")
	return nil
}

func TestPageList_SortsAndPagesByKey(t *testing.T) {
	const budget = 4096
	rows := make([]*pageRow, 0, 8)
	for _, name := range []string{"e", "a", "d", "b", "c", "f", "h", "g"} {
		rows = append(rows, pageRowOf("ns", name, 900))
	}
	got := walkPages(t, rows, budget)
	want := []string{"ns/a", "ns/b", "ns/c", "ns/d", "ns/e", "ns/f", "ns/g", "ns/h"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", got, want)
	}

	page, next, err := pageList(rows, "", budget, marshalPageRow)
	if err != nil {
		t.Fatal(err)
	}
	if next == "" || len(page) >= len(rows) {
		t.Fatalf("expected a partial first page, got %d rows next=%q", len(page), next)
	}
	size := 0
	for _, raw := range page {
		size += len(raw) + 1
	}
	if size > budget-1024 {
		t.Fatalf("page holds %d bytes, over the %d byte limit", size, budget-1024)
	}
}

func TestPageList_AfterSkipsEarlierKeys(t *testing.T) {
	rows := []*pageRow{pageRowOf("ns", "a", 10), pageRowOf("ns", "b", 10), pageRowOf("ns", "c", 10)}
	page, next, err := pageList(rows, "ns/b", 4096, marshalPageRow)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || next != "" {
		t.Fatalf("got %d rows next=%q, want 1 row and no next", len(page), next)
	}
	var row struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(page[0], &row); err != nil {
		t.Fatal(err)
	}
	if row.Key != "ns/c" {
		t.Fatalf("first row after ns/b = %q, want ns/c", row.Key)
	}
}

func TestPageList_OversizedFirstRowFails(t *testing.T) {
	rows := []*pageRow{pageRowOf("ns", "big", 5000)}
	_, _, err := pageList(rows, "", 4096, marshalPageRow)
	var tooLarge *listRowTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want *listRowTooLargeError", err)
	}
	if tooLarge.Key != "ns/big" {
		t.Fatalf("key = %q, want ns/big", tooLarge.Key)
	}
}

func TestPageList_OversizedLaterRowEndsPage(t *testing.T) {
	const budget = 4096
	rows := []*pageRow{pageRowOf("ns", "a", 10), pageRowOf("ns", "b", 5000)}
	page, next, err := pageList(rows, "", budget, marshalPageRow)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(page) != 1 || next != "ns/a" {
		t.Fatalf("first page = %d rows next=%q, want 1 row and next ns/a", len(page), next)
	}
	_, _, err = pageList(rows, next, budget, marshalPageRow)
	var tooLarge *listRowTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Key != "ns/b" {
		t.Fatalf("second page err = %v, want too large on ns/b", err)
	}
}

func TestPageList_LastPageHasNoNext(t *testing.T) {
	rows := []*pageRow{pageRowOf("ns", "a", 10), pageRowOf("ns", "b", 10)}
	page, next, err := pageList(rows, "", 4096, marshalPageRow)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || next != "" {
		t.Fatalf("got %d rows next=%q, want 2 rows and no next", len(page), next)
	}
	empty, next, err := pageList([]*pageRow{}, "", 4096, marshalPageRow)
	if err != nil || empty == nil || len(empty) != 0 || next != "" {
		t.Fatalf("empty input = %v next=%q err=%v, want empty non-nil page", empty, next, err)
	}
}

func TestPageList_ClusterScopedKey(t *testing.T) {
	rows := []*pageRow{pageRowOf("", "b", 10), pageRowOf("", "a", 10)}
	got := walkPages(t, rows, 4096)
	if strings.Join(got, ",") != "/a,/b" {
		t.Fatalf("keys = %v, want [/a /b]", got)
	}
	page, _, err := pageList(rows, "/a", 4096, marshalPageRow)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 {
		t.Fatalf("rows after /a = %d, want 1", len(page))
	}
}
