package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/varroaci/varroa-jenkins/internal/bootsecret"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
)

const testSigningNS = "varroa-system"

func signingKeySecret(t *testing.T, pem string) *corev1.Secret {
	t.Helper()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: miteSigningKeySecretName, Namespace: testSigningNS},
		Data:       map[string][]byte{"private-key": []byte(pem)},
	}
}

func freshSigningPEM(t *testing.T) (string, string) {
	t.Helper()
	s, err := mitesrv.NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	pem, err := s.PrivateKeyPEM()
	if err != nil {
		t.Fatalf("PrivateKeyPEM: %v", err)
	}
	return pem, s.KID()
}

// assertNoSecretWrites fails when the loader wrote the Secret. The BFF reads
// the signing key and never creates or changes it.
func assertNoSecretWrites(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	for _, a := range cs.Actions() {
		switch a.GetVerb() {
		case "create", "update", "patch":
			if a.GetResource().Resource == "secrets" {
				t.Fatalf("loader wrote a Secret: %s", a.GetVerb())
			}
		}
	}
}

func TestLoadMiteSigningKey_LoadsWhenPresent(t *testing.T) {
	pem, kid := freshSigningPEM(t)
	cs := fake.NewSimpleClientset(signingKeySecret(t, pem))
	signer, err := loadMiteSigningKey(context.Background(), cs, testSigningNS, time.Second, 10*time.Millisecond, slog.Default())
	if err != nil {
		t.Fatalf("loadMiteSigningKey: %v", err)
	}
	if signer.KID() != kid {
		t.Fatalf("KID = %q, want %q", signer.KID(), kid)
	}
	assertNoSecretWrites(t, cs)
}

func TestLoadMiteSigningKey_WaitsForTheOperator(t *testing.T) {
	pem, kid := freshSigningPEM(t)
	cs := fake.NewSimpleClientset(signingKeySecret(t, pem))
	calls := 0
	cs.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls <= 3 {
			return true, nil, apierrors.NewNotFound(corev1.Resource("secrets"), miteSigningKeySecretName)
		}
		return false, nil, nil
	})
	signer, err := loadMiteSigningKey(context.Background(), cs, testSigningNS, time.Second, 10*time.Millisecond, slog.Default())
	if err != nil {
		t.Fatalf("loadMiteSigningKey: %v", err)
	}
	if signer.KID() != kid {
		t.Fatalf("KID = %q, want %q", signer.KID(), kid)
	}
	gets := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == "secrets" {
			gets++
		}
	}
	if gets < 4 {
		t.Fatalf("get actions = %d, want at least 4", gets)
	}
	assertNoSecretWrites(t, cs)
}

func TestLoadMiteSigningKey_AbsentAtDeadlineFails(t *testing.T) {
	cs := fake.NewSimpleClientset()
	const timeout = 40 * time.Millisecond
	start := time.Now()
	signer, err := loadMiteSigningKey(context.Background(), cs, testSigningNS, timeout, 5*time.Millisecond, slog.Default())
	if err == nil {
		t.Fatal("expected an error when the Secret stays absent")
	}
	if !errors.Is(err, bootsecret.ErrNotFound) {
		t.Fatalf("error %v, want wrapped bootsecret.ErrNotFound", err)
	}
	if signer != nil {
		t.Fatal("signer must be nil on failure")
	}
	if elapsed := time.Since(start); elapsed < timeout {
		t.Fatalf("returned after %v, want at least %v", elapsed, timeout)
	}
	assertNoSecretWrites(t, cs)
}

func TestLoadMiteSigningKey_ReadErrorIsNotAbsence(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), miteSigningKeySecretName, errors.New("injected"))
	})
	_, err := loadMiteSigningKey(context.Background(), cs, testSigningNS, 30*time.Millisecond, 5*time.Millisecond, slog.Default())
	if err == nil {
		t.Fatal("expected an error on a persistent read error")
	}
	if errors.Is(err, bootsecret.ErrNotFound) {
		t.Fatal("a read error must not report ErrNotFound")
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("error %v, want a wrapped Forbidden error", err)
	}
	assertNoSecretWrites(t, cs)
}

func TestLoadMiteSigningKey_IncompleteSecretFails(t *testing.T) {
	s := signingKeySecret(t, "")
	s.Data["private-key"] = []byte{}
	cs := fake.NewSimpleClientset(s)
	_, err := loadMiteSigningKey(context.Background(), cs, testSigningNS, 30*time.Millisecond, 5*time.Millisecond, slog.Default())
	if err == nil {
		t.Fatal("expected an error for an incomplete Secret")
	}
	var inc *bootsecret.IncompleteError
	if !errors.As(err, &inc) {
		t.Fatalf("error %v, want *bootsecret.IncompleteError", err)
	}
	if inc.Key != "private-key" {
		t.Fatalf("incomplete key = %q, want private-key", inc.Key)
	}
	assertNoSecretWrites(t, cs)
}

func TestLoadMiteSigningKey_BadPEMFails(t *testing.T) {
	cs := fake.NewSimpleClientset(signingKeySecret(t, "not a pem"))
	_, err := loadMiteSigningKey(context.Background(), cs, testSigningNS, time.Second, 5*time.Millisecond, slog.Default())
	if err == nil {
		t.Fatal("expected an error for a bad PEM")
	}
	if errors.Is(err, bootsecret.ErrNotFound) {
		t.Fatal("a bad PEM must not report ErrNotFound")
	}
	if !strings.Contains(err.Error(), miteSigningKeySecretName) {
		t.Fatalf("error %q does not name the Secret", err.Error())
	}
	assertNoSecretWrites(t, cs)
}
