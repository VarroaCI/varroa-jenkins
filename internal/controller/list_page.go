package controller

import (
	"encoding/json"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// listRowTooLargeError reports a row whose JSON alone exceeds the page budget.
type listRowTooLargeError struct{ Key string }

func (e *listRowTooLargeError) Error() string {
	return "row " + e.Key + " exceeds the list page budget"
}

// listPageKey is the paging key of a row: "namespace/name". A cluster-scoped
// row has an empty namespace.
func listPageKey(o metav1.Object) string {
	return o.GetNamespace() + "/" + o.GetName()
}

// pageList sorts items by key, skips keys at or below after (when after is
// not empty), and marshals the rest in key order. marshal runs lazily, so a
// caller that does extra reads per row pays for the rows on the page only.
// The page keeps whole rows while the reply stays under budget minus 1024
// bytes of envelope. next is the key of the last kept row when a row is left
// out, and empty on the last page. A first remaining row that is over the
// budget alone returns *listRowTooLargeError. A marshal error is returned as is.
func pageList[T metav1.Object](items []T, after string, budget int, marshal func(T) (json.RawMessage, error)) (rows []json.RawMessage, next string, err error) {
	sorted := make([]T, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		return listPageKey(sorted[i]) < listPageKey(sorted[j])
	})

	limit := budget - 1024
	rows = make([]json.RawMessage, 0)
	size := 0
	lastKey := ""
	for _, it := range sorted {
		key := listPageKey(it)
		if after != "" && key <= after {
			continue
		}
		raw, err := marshal(it)
		if err != nil {
			return nil, "", err
		}
		if len(rows) == 0 && len(raw)+1 > limit {
			return nil, "", &listRowTooLargeError{Key: key}
		}
		if size+len(raw)+1 > limit {
			return rows, lastKey, nil
		}
		rows = append(rows, raw)
		size += len(raw) + 1
		lastKey = key
	}
	return rows, "", nil
}
