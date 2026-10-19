package sealedkey

import (
	"strings"
	"testing"
)

func newSealer(t *testing.T, pem string) *Sealer {
	t.Helper()
	s, err := New(pem)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	s := newSealer(t, "pem-a")
	v, err := s.Seal("sub-1", 100, "anthropic-main", []byte("secret-key"))
	if err != nil {
		t.Fatal(err)
	}
	name, key, ok := s.Open(v, "sub-1", 100)
	if !ok || name != "anthropic-main" || key.Empty() {
		t.Fatalf("Open = %q, %v, %v", name, key, ok)
	}
	if strings.Contains(key.String(), "secret") {
		t.Errorf("key prints its bytes")
	}
}

func TestOtherIatOpensNothing(t *testing.T) {
	s := newSealer(t, "pem-a")
	v, _ := s.Seal("sub-1", 100, "p", []byte("k"))
	if _, _, ok := s.Open(v, "sub-1", 101); ok {
		t.Error("a value opened for another login session")
	}
}

func TestOtherSubjectOpensNothing(t *testing.T) {
	s := newSealer(t, "pem-a")
	v, _ := s.Seal("sub-1", 100, "p", []byte("k"))
	if _, _, ok := s.Open(v, "sub-2", 100); ok {
		t.Error("a value opened for another subject")
	}
}

func TestTwoInstancesOneKey(t *testing.T) {
	a := newSealer(t, "shared-pem")
	b := newSealer(t, "shared-pem")
	v, _ := a.Seal("sub-1", 7, "p", []byte("k"))
	if _, _, ok := b.Open(v, "sub-1", 7); !ok {
		t.Error("a second replica with the same key cannot open the value")
	}
	c := newSealer(t, "other-pem")
	if _, _, ok := c.Open(v, "sub-1", 7); ok {
		t.Error("a replica with another key opened the value")
	}
}

func TestTamperedValue(t *testing.T) {
	s := newSealer(t, "pem-a")
	v, _ := s.Seal("sub-1", 100, "p", []byte("k"))
	tampered := []byte(v)
	tampered[len(tampered)-2] ^= 0x01
	if _, _, ok := s.Open(string(tampered), "sub-1", 100); ok {
		t.Error("a tampered value opened")
	}
	if _, _, ok := s.Open("!!not-base64!!", "sub-1", 100); ok {
		t.Error("a malformed value opened")
	}
}
