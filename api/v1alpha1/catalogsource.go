package v1alpha1

import "errors"

// ErrReservedCatalogSource refuses a create, update, or delete of the reserved
// update-center source. The operator creates it and re-asserts its spec on
// every tick.
var ErrReservedCatalogSource = errors.New("the reserved update-center source is created and reconciled by the operator and cannot be created, edited, or deleted")

// CheckCatalogSourceWritable is the single write guard for CatalogSource
// names. Every create, update, and delete path calls it. Sync does not.
func CheckCatalogSourceWritable(name string) error {
	if name == UpdateCenterCatalogSourceName {
		return ErrReservedCatalogSource
	}
	return nil
}
