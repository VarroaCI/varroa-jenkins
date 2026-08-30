package controller

import "testing"

// TestCascContentHash_Deterministic verifies that hashing the same CASC
// content twice (with map keys inserted in a different order) always
// produces the same hash.
func TestCascContentHash_Deterministic(t *testing.T) {
	a := map[string]string{
		"realm.yaml":  "realm-content",
		"config.yaml": "config-content",
		"rbac.yaml":   "rbac-content",
	}
	b := map[string]string{
		"rbac.yaml":   "rbac-content",
		"config.yaml": "config-content",
		"realm.yaml":  "realm-content",
	}

	hashA := cascContentHash(a)
	hashB := cascContentHash(b)

	if hashA == "" {
		t.Fatal("expected non-empty hash")
	}
	if hashA != hashB {
		t.Fatalf("expected identical hashes for identical content regardless of map insertion order, got %q vs %q", hashA, hashB)
	}
}

// TestCascContentHash_ChangesWithContent verifies that any change to the
// CASC payload (a changed value, an added key, or a removed key) changes
// the hash.
func TestCascContentHash_ChangesWithContent(t *testing.T) {
	base := map[string]string{
		"realm.yaml":  "realm-content",
		"config.yaml": "config-content",
	}
	baseHash := cascContentHash(base)

	changedValue := map[string]string{
		"realm.yaml":  "realm-content",
		"config.yaml": "config-content-CHANGED",
	}
	if h := cascContentHash(changedValue); h == baseHash {
		t.Fatal("expected a changed value to change the hash")
	}

	addedKey := map[string]string{
		"realm.yaml":  "realm-content",
		"config.yaml": "config-content",
		"rbac.yaml":   "rbac-content",
	}
	if h := cascContentHash(addedKey); h == baseHash {
		t.Fatal("expected an added key to change the hash")
	}

	removedKey := map[string]string{
		"config.yaml": "config-content",
	}
	if h := cascContentHash(removedKey); h == baseHash {
		t.Fatal("expected a removed key to change the hash")
	}
}

// TestCascContentHash_NoDelimiterCollision pins a case a delimiter-joined
// concatenation would get wrong: shifting a NUL-joined boundary from inside
// a value into a key/value separator must not produce the same hash as a
// differently-shaped map whose concatenation happens to match.
func TestCascContentHash_NoDelimiterCollision(t *testing.T) {
	shiftedBoundary := map[string]string{
		"a": "x\x00b\x00y",
	}
	splitAcrossKeys := map[string]string{
		"a": "x",
		"b": "y",
	}

	hashShifted := cascContentHash(shiftedBoundary)
	hashSplit := cascContentHash(splitAcrossKeys)

	if hashShifted == hashSplit {
		t.Fatalf("expected distinct maps to hash differently, both hashed to %q", hashShifted)
	}
}
