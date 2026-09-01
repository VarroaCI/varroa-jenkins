package pluginresolve

import (
	"archive/zip"
	"bytes"
	"errors"
	"testing"
)

// buildHPIWithCore builds a genuine .hpi holding a manifest that declares a
// Jenkins-Version (RequiredCore), which fixtureHPI (bootstrap_test.go) never
// sets.
func buildHPIWithCore(t *testing.T, requiredCore string) []byte {
	t.Helper()
	mf := "Manifest-Version: 1.0\r\nShort-Name: varroa-mite-auth\r\nPlugin-Version: 1.0\r\n"
	if requiredCore != "" {
		mf += "Jenkins-Version: " + requiredCore + "\r\n"
	}
	mf += "\r\n"

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("META-INF/MANIFEST.MF")
	if err != nil {
		t.Fatalf("create manifest entry: %v", err)
	}
	if _, err := w.Write([]byte(mf)); err != nil {
		t.Fatalf("write manifest entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestAssertCoreFloor_MalformedHPIWrapsParseError(t *testing.T) {
	err := AssertCoreFloor("2.479.3", []byte("not a zip archive"))
	if err == nil {
		t.Fatal("expected an error for malformed HPI bytes")
	}
	if errors.Is(err, ErrInvalidVersion) || errors.Is(err, ErrRootCoreFloorExceeded) {
		t.Errorf("a parse failure must be wrapped as-is, not mapped to a Resolve sentinel, got: %v", err)
	}
}

func TestAssertCoreFloor_EmptyRequiredCorePasses(t *testing.T) {
	root := fixtureHPI(t, "varroa-mite-auth", "1.0", "") // no Jenkins-Version line
	if err := AssertCoreFloor("2.479.3", root); err != nil {
		t.Fatalf("AssertCoreFloor: %v", err)
	}
}

func TestAssertCoreFloor_RequiredCoreWithinTargetPasses(t *testing.T) {
	root := buildHPIWithCore(t, "2.400")
	if err := AssertCoreFloor("2.479.3", root); err != nil {
		t.Fatalf("AssertCoreFloor: %v", err)
	}
}

func TestAssertCoreFloor_RequiredCoreExceedsTargetFails(t *testing.T) {
	root := buildHPIWithCore(t, "2.999")
	err := AssertCoreFloor("2.479.3", root)
	if !errors.Is(err, ErrRootCoreFloorExceeded) {
		t.Fatalf("err = %v, want ErrRootCoreFloorExceeded", err)
	}
}

func TestAssertCoreFloor_UnparseableVersionFails(t *testing.T) {
	t.Run("unparseable target", func(t *testing.T) {
		root := buildHPIWithCore(t, "2.400")
		err := AssertCoreFloor("not-a-version", root)
		if !errors.Is(err, ErrInvalidVersion) {
			t.Fatalf("err = %v, want ErrInvalidVersion", err)
		}
	})
	t.Run("unparseable RequiredCore", func(t *testing.T) {
		root := buildHPIWithCore(t, "not-a-version")
		err := AssertCoreFloor("2.479.3", root)
		if !errors.Is(err, ErrInvalidVersion) {
			t.Fatalf("err = %v, want ErrInvalidVersion", err)
		}
	})
}
