package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/varroaci/varroa-jenkins/internal/ca"
)

func caSecret(ns string, t *testing.T) (*corev1.Secret, *ca.CA) {
	t.Helper()
	authority, err := ca.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	certPEM, keyPEM, err := authority.Persist()
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: caSecretName, Namespace: ns},
		Data:       map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM},
	}, authority
}

func TestWaitForCASecret_LoadsWhenPresent(t *testing.T) {
	secret, authority := caSecret("varroa-system", t)
	client := fake.NewSimpleClientset(secret)

	got, err := waitForCASecret(context.Background(), client, "varroa-system", time.Second, 10*time.Millisecond, slog.Default())
	if err != nil {
		t.Fatalf("waitForCASecret: %v", err)
	}
	if got == nil {
		t.Fatal("expected CA loaded from Secret, got nil")
	}
	if string(got.CAPEM()) != string(authority.CAPEM()) {
		t.Fatal("loaded CA does not match the Secret's CA")
	}
}

func TestWaitForCASecret_TimesOutWhenAbsent(t *testing.T) {
	client := fake.NewSimpleClientset() // no varroa-ca Secret

	start := time.Now()
	got, err := waitForCASecret(context.Background(), client, "varroa-system", 40*time.Millisecond, 10*time.Millisecond, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil CA (never fall back to an ephemeral CA here) when Secret absent")
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("expected to wait until the deadline before giving up")
	}
}

func TestWaitForCASecret_LoadsWhenSecretAppearsLate(t *testing.T) {
	client := fake.NewSimpleClientset()
	secret, _ := caSecret("varroa-system", t)

	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _ = client.CoreV1().Secrets("varroa-system").Create(context.Background(), secret, metav1.CreateOptions{})
	}()

	got, err := waitForCASecret(context.Background(), client, "varroa-system", 2*time.Second, 10*time.Millisecond, slog.Default())
	if err != nil {
		t.Fatalf("waitForCASecret: %v", err)
	}
	if got == nil {
		t.Fatal("expected CA to load once the Secret appears mid-wait")
	}
}
