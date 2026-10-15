package v1alpha1

import (
	"errors"
	"testing"
)

func TestCheckCatalogSourceWritable(t *testing.T) {
	if err := CheckCatalogSourceWritable(UpdateCenterCatalogSourceName); !errors.Is(err, ErrReservedCatalogSource) {
		t.Fatalf("reserved name: got %v, want ErrReservedCatalogSource", err)
	}
	for _, name := range []string{"src", ""} {
		if err := CheckCatalogSourceWritable(name); err != nil {
			t.Errorf("CheckCatalogSourceWritable(%q) = %v, want nil", name, err)
		}
	}
}
