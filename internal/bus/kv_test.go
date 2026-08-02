package bus

import (
	"errors"
	"testing"
	"time"
)

func TestKVCreateAtomic(t *testing.T) {
	s := startTestServer(t)
	c := connectTest(t, s)

	kv, err := c.EnsureKV("create_test", time.Minute)
	if err != nil {
		t.Fatalf("EnsureKV: %v", err)
	}

	if err := kv.Create("k", []byte("v1")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err = kv.Create("k", []byte("v2"))
	if !errors.Is(err, ErrKVKeyExists) {
		t.Fatalf("second Create: want ErrKVKeyExists, got %v", err)
	}
	if err := kv.Create("k2", []byte("v")); err != nil {
		t.Fatalf("Create distinct key: %v", err)
	}

	got, err := kv.Get("k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v1" {
		t.Fatalf("value overwritten by failed Create: got %q", got)
	}
}
