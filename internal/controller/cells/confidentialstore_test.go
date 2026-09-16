package cells

import (
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// mustDecrypt answers the payload of one encrypted key, and fails the test
// on a ciphertext DecryptKey refuses.
func mustDecrypt(t *testing.T, masterKeyHex string, ciphertext []byte) []byte {
	t.Helper()
	payload, err := DecryptKey(masterKeyHex, ciphertext)
	if err != nil {
		t.Fatalf("DecryptKey: %v", err)
	}
	return payload
}

// TestNewMasterKey_Format asserts the master.key text format: 256 lowercase
// hexadecimal characters and no trailing newline.
func TestNewMasterKey_Format(t *testing.T) {
	master := NewMasterKey()
	if len(master) != 2*masterKeyRandomBytes {
		t.Fatalf("master.key is %d characters, want %d", len(master), 2*masterKeyRandomBytes)
	}
	if strings.TrimRight(master, "\r\n") != master {
		t.Fatal("master.key carries a trailing newline")
	}
	if _, err := hex.DecodeString(master); err != nil {
		t.Fatalf("master.key is not hexadecimal: %v", err)
	}
	if master != strings.ToLower(master) {
		t.Fatal("master.key is not lowercase")
	}
	if master == NewMasterKey() {
		t.Fatal("two master keys are equal")
	}
}

// TestMintStore_KeysAndFormat asserts that a minted store holds the four
// keys, that each ciphertext is a whole number of AES blocks, and that a
// round trip recovers the payload and the trailer.
func TestMintStore_KeysAndFormat(t *testing.T) {
	store, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	if len(store) != len(StoreKeys()) {
		t.Fatalf("the store holds %d keys, want %d", len(store), len(StoreKeys()))
	}
	for _, key := range StoreKeys() {
		if len(store[key]) == 0 {
			t.Fatalf("the store lacks the key %q", key)
		}
	}
	master := string(store[KeyMasterKey])
	for key, want := range encryptedKeyPayloadBytes {
		value := store[key]
		if len(value)%aes.BlockSize != 0 {
			t.Errorf("%s is %d bytes, which is not a whole number of blocks", key, len(value))
		}
		payload := mustDecrypt(t, master, value)
		if len(payload) != want {
			t.Errorf("%s carries a payload of %d bytes, want %d", key, len(payload), want)
		}
	}
}

// TestMintStore_EachPassIsDifferent asserts that two pools never share a key
// value.
func TestMintStore_EachPassIsDifferent(t *testing.T) {
	first, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	second, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	for _, key := range StoreKeys() {
		if bytes.Equal(first[key], second[key]) {
			t.Errorf("two stores share the key %q", key)
		}
	}
}

// TestRepairStore_AddsOnlyTheMissingKeys asserts that a repair adds each
// absent key under the store's own master.key and changes no present key.
func TestRepairStore_AddsOnlyTheMissingKeys(t *testing.T) {
	full, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	existing := map[string][]byte{
		KeyMasterKey:  full[KeyMasterKey],
		KeyJnlpSecret: full[KeyJnlpSecret],
	}
	added, err := RepairStore(existing)
	if err != nil {
		t.Fatalf("RepairStore: %v", err)
	}
	if len(added) != 2 {
		t.Fatalf("the repair added %d keys, want 2", len(added))
	}
	for _, key := range []string{KeyHudsonUtilSecret, KeyInstanceIdentity} {
		value := added[key]
		if len(value) == 0 {
			t.Fatalf("the repair did not add %q", key)
		}
		if _, err := DecryptKey(string(full[KeyMasterKey]), value); err != nil {
			t.Errorf("the repaired %s does not decrypt: %v", key, err)
		}
	}
	if _, present := added[KeyMasterKey]; present {
		t.Error("the repair rewrote master.key")
	}
	if _, present := added[KeyJnlpSecret]; present {
		t.Error("the repair rewrote the jnlp secret")
	}
}

// TestRepairStore_CompleteStoreAddsNothing asserts that a store with every
// key gets no write.
func TestRepairStore_CompleteStoreAddsNothing(t *testing.T) {
	full, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	added, err := RepairStore(full)
	if err != nil {
		t.Fatalf("RepairStore: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("the repair added %d keys, want none", len(added))
	}
}

// TestRepairStore_RefusesAStoreWithNoMasterKey asserts the one state an
// operator must end by hand.
func TestRepairStore_RefusesAStoreWithNoMasterKey(t *testing.T) {
	full, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	_, err = RepairStore(map[string][]byte{KeyJnlpSecret: full[KeyJnlpSecret]})
	if !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("RepairStore answered %v, want ErrNoMasterKey", err)
	}
}

// TestEncryptKey_TrailerAndPadding asserts that a payload whose length is
// already a whole number of blocks still gains a full padding block.
func TestEncryptKey_TrailerAndPadding(t *testing.T) {
	master := NewMasterKey()
	payload := bytes.Repeat([]byte{7}, 3)
	value, err := EncryptKey(master, payload)
	if err != nil {
		t.Fatalf("EncryptKey: %v", err)
	}
	// The payload plus the 13-byte trailer is exactly one block, so the
	// cipher writes a second block of padding.
	if len(value) != 2*aes.BlockSize {
		t.Fatalf("the ciphertext is %d bytes, want %d", len(value), 2*aes.BlockSize)
	}
	if got := mustDecrypt(t, master, value); !bytes.Equal(got, payload) {
		t.Fatal("the round trip did not recover the payload")
	}
}

// TestPoolSecretName asserts the store's name.
func TestPoolSecretName(t *testing.T) {
	if got := PoolSecretName("lts-2-555"); got != "varroa-cell-secrets-lts-2-555" {
		t.Fatalf("PoolSecretName = %q", got)
	}
}

// TestVerifyStore_AcceptsAMintedStore asserts the happy path.
func TestVerifyStore_AcceptsAMintedStore(t *testing.T) {
	store, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	if err := VerifyStore(store); err != nil {
		t.Fatalf("VerifyStore: %v", err)
	}
}

// TestVerifyStore_AcceptsATrailingNewline asserts that a hand edit which
// adds whitespace around master.key still verifies, because Jenkins reads
// that file with readTrim.
func TestVerifyStore_AcceptsATrailingNewline(t *testing.T) {
	store, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	store[KeyMasterKey] = append(store[KeyMasterKey], '\n')
	if err := VerifyStore(store); err != nil {
		t.Fatalf("VerifyStore: %v", err)
	}
}

// TestVerifyStore_RefusesAFlippedByte asserts that a changed byte in the
// last block of a key is caught. Jenkins answers null for such a file and
// silently generates a fresh key, so the presence test alone is not enough.
// The cipher is ECB, so only the blocks that carry the trailer and the
// padding can report a change; a byte flipped in an earlier block gives a
// file Jenkins itself accepts, and every cell of the pool then reads the
// same wrong payload rather than diverging.
func TestVerifyStore_RefusesAFlippedByte(t *testing.T) {
	store, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	value := store[KeyJnlpSecret]
	value[len(value)-1] ^= 0xff
	err = VerifyStore(store)
	if err == nil {
		t.Fatal("VerifyStore accepted a key with a flipped byte")
	}
	if !strings.Contains(err.Error(), KeyJnlpSecret) {
		t.Errorf("the error does not name the key: %v", err)
	}
}

// TestVerifyStore_RefusesAKeyOfAnotherStore asserts that a key encrypted
// under a different master.key is caught.
func TestVerifyStore_RefusesAKeyOfAnotherStore(t *testing.T) {
	store, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	other, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	store[KeyHudsonUtilSecret] = other[KeyHudsonUtilSecret]
	if err := VerifyStore(store); err == nil {
		t.Fatal("VerifyStore accepted a key of another store")
	}
}

// TestVerifyStore_RefusesAMisalignedKey asserts that a truncated ciphertext
// is caught before the cipher runs.
func TestVerifyStore_RefusesAMisalignedKey(t *testing.T) {
	store, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	store[KeyInstanceIdentity] = store[KeyInstanceIdentity][:len(store[KeyInstanceIdentity])-1]
	if err := VerifyStore(store); err == nil {
		t.Fatal("VerifyStore accepted a ciphertext that is not a whole number of blocks")
	}
}

// TestVerifyStore_RefusesAStoreWithNoMasterKey asserts the fault an operator
// must end by hand.
func TestVerifyStore_RefusesAStoreWithNoMasterKey(t *testing.T) {
	store, err := MintStore()
	if err != nil {
		t.Fatalf("MintStore: %v", err)
	}
	delete(store, KeyMasterKey)
	if err := VerifyStore(store); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("VerifyStore answered %v, want ErrNoMasterKey", err)
	}
}
