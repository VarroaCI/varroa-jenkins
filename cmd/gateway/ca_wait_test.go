package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/varroaci/varroa-jenkins/internal/bootsecret"
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
	if !errors.Is(err, bootsecret.ErrNotFound) {
		t.Fatalf("error %v, want one wrapping bootsecret.ErrNotFound", err)
	}
	if got != nil {
		t.Fatal("expected nil CA when the Secret is absent")
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("expected to wait until the deadline before giving up")
	}
}

func TestWaitForCASecret_ReadErrorIsNotAbsence(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected read failure")
	})

	got, err := waitForCASecret(context.Background(), client, "varroa-system", 40*time.Millisecond, 10*time.Millisecond, slog.Default())
	if err == nil || errors.Is(err, bootsecret.ErrNotFound) {
		t.Fatalf("error %v, want a read error that is not ErrNotFound", err)
	}
	if got != nil {
		t.Fatal("expected nil CA on a read error")
	}
}

func TestLoadGatewayCA_ReadErrorNeverMintsEphemeral(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected read failure")
	})

	got, err := loadGatewayCA(context.Background(), client, "varroa-system", true,
		40*time.Millisecond, 10*time.Millisecond, slog.Default())
	if err == nil {
		t.Fatal("expected an error: a read error must never mint, even with allowEphemeral")
	}
	if got != nil {
		t.Fatal("expected nil CA")
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" {
			t.Fatal("gateway must not create the Secret")
		}
	}
}

func TestLoadGatewayCA_AbsentWithFlagMintsEphemeral(t *testing.T) {
	client := fake.NewSimpleClientset()

	got, err := loadGatewayCA(context.Background(), client, "varroa-system", true,
		40*time.Millisecond, 10*time.Millisecond, slog.Default())
	if err != nil {
		t.Fatalf("loadGatewayCA: %v", err)
	}
	if got == nil {
		t.Fatal("expected an ephemeral CA")
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" {
			t.Fatal("gateway must not create the Secret")
		}
	}
}

func TestLoadGatewayCA_AbsentWithoutFlagFails(t *testing.T) {
	client := fake.NewSimpleClientset()

	got, err := loadGatewayCA(context.Background(), client, "varroa-system", false,
		40*time.Millisecond, 10*time.Millisecond, slog.Default())
	if !errors.Is(err, bootsecret.ErrNotFound) {
		t.Fatalf("error %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "VARROA_ALLOW_EPHEMERAL_CA") {
		t.Fatalf("error %q does not name the override", err)
	}
	if got != nil {
		t.Fatal("expected nil CA")
	}
}

func TestLoadGatewayCA_NilClient(t *testing.T) {
	got, err := loadGatewayCA(context.Background(), nil, "varroa-system", true,
		time.Second, 10*time.Millisecond, slog.Default())
	if err != nil || got == nil {
		t.Fatalf("with the flag, want an ephemeral CA: got %v, err %v", got, err)
	}

	if _, err := loadGatewayCA(context.Background(), nil, "varroa-system", false,
		time.Second, 10*time.Millisecond, slog.Default()); err == nil {
		t.Fatal("without the flag, a nil client must fail")
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
