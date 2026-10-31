package main

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/varroaci/varroa-jenkins/internal/bootsecret"
	"github.com/varroaci/varroa-jenkins/internal/ca"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
)

const startupTestNS = "varroa-system"

var startupSecretGVR = corev1.SchemeGroupVersion.WithResource("secrets")

func startupOpts() bootsecret.Options {
	return bootsecret.Options{Poll: 10 * time.Millisecond}
}

func startupCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func startupCountVerb(cs *fake.Clientset, verb string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb {
			n++
		}
	}
	return n
}

func startupAssertNoWrites(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	if n := startupCountVerb(cs, "create") + startupCountVerb(cs, "update") + startupCountVerb(cs, "patch"); n != 0 {
		t.Fatalf("writes = %d, want 0", n)
	}
}

// startupFailFirstGets makes the first n Get calls fail with a transient error.
func startupFailFirstGets(cs *fake.Clientset, n int) {
	var mu sync.Mutex
	left := n
	cs.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if left > 0 {
			left--
			return true, nil, errors.New("net/http: TLS handshake timeout")
		}
		return false, nil, nil
	})
}

// startupFailAllGets makes every Get fail with a transient error.
func startupFailAllGets(cs *fake.Clientset) {
	cs.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("net/http: TLS handshake timeout")
	})
}

// startupRaceCreate makes the first Create lose a race to a competitor Secret.
func startupRaceCreate(cs *fake.Clientset, name string, data map[string][]byte) {
	var once sync.Once
	cs.PrependReactor("create", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		var handled bool
		once.Do(func() {
			_ = cs.Tracker().Create(startupSecretGVR, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: startupTestNS},
				Data:       data,
			}, startupTestNS)
			handled = true
		})
		if handled {
			return true, nil, apierrors.NewAlreadyExists(corev1.Resource("secrets"), name)
		}
		return false, nil, nil
	})
}

func storedCASecret(t *testing.T) (*corev1.Secret, *ca.CA) {
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
		ObjectMeta: metav1.ObjectMeta{Name: caSecretName, Namespace: startupTestNS},
		Data:       map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM},
	}, authority
}

func storedKeySecret(t *testing.T) (*corev1.Secret, *mitesrv.MiteTokenSigner) {
	t.Helper()
	signer, err := mitesrv.NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	privPEM, err := signer.PrivateKeyPEM()
	if err != nil {
		t.Fatalf("PrivateKeyPEM: %v", err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: miteSigningKeySecretName, Namespace: startupTestNS},
		Data:       map[string][]byte{"private-key": []byte(privPEM)},
	}, signer
}

func TestLoadOrCreateCA_TransientReadErrorKeepsStoredCA(t *testing.T) {
	secret, stored := storedCASecret(t)
	cs := fake.NewSimpleClientset(secret)
	startupFailFirstGets(cs, 1)

	got, created, err := loadOrCreateCA(startupCtx(t, 2*time.Second), cs, startupTestNS, startupOpts())
	if err != nil {
		t.Fatalf("loadOrCreateCA: %v", err)
	}
	if created {
		t.Fatal("created = true for a stored CA")
	}
	if !bytes.Equal(got.CAPEM(), stored.CAPEM()) {
		t.Fatal("loaded CA does not match the stored CA")
	}
	if !bytes.Equal(got.BootstrapHMACKey(), stored.BootstrapHMACKey()) {
		t.Fatal("bootstrap HMAC key does not match the stored CA")
	}
	startupAssertNoWrites(t, cs)
}

func TestLoadOrCreateCA_ReadFailureNeverCreatesOrOverwrites(t *testing.T) {
	secret, _ := storedCASecret(t)
	cs := fake.NewSimpleClientset(secret)
	startupFailAllGets(cs)

	_, _, err := loadOrCreateCA(startupCtx(t, 100*time.Millisecond), cs, startupTestNS, startupOpts())
	if err == nil {
		t.Fatal("expected an error when the read never succeeds")
	}
	startupAssertNoWrites(t, cs)
}

func TestLoadOrCreateCA_CreatesOnFirstInstall(t *testing.T) {
	cs := fake.NewSimpleClientset()

	got, created, err := loadOrCreateCA(startupCtx(t, 2*time.Second), cs, startupTestNS, startupOpts())
	if err != nil {
		t.Fatalf("loadOrCreateCA: %v", err)
	}
	if !created {
		t.Fatal("created = false on first install")
	}
	stored, err := cs.CoreV1().Secrets(startupTestNS).Get(context.Background(), caSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(stored.Data["tls.crt"], got.CAPEM()) {
		t.Fatal("stored tls.crt does not equal the returned CA")
	}
}

func TestLoadOrCreateCA_AlreadyExistsAdoptsStoredCA(t *testing.T) {
	competitor, competitorCA := storedCASecret(t)
	cs := fake.NewSimpleClientset()
	startupRaceCreate(cs, caSecretName, competitor.Data)

	got, created, err := loadOrCreateCA(startupCtx(t, 2*time.Second), cs, startupTestNS, startupOpts())
	if err != nil {
		t.Fatalf("loadOrCreateCA: %v", err)
	}
	if created {
		t.Fatal("created = true when another replica won the race")
	}
	if !bytes.Equal(got.CAPEM(), competitorCA.CAPEM()) {
		t.Fatal("returned CA is not the competitor's CA")
	}
}

func TestLoadOrCreateMiteSigningKey_TransientReadErrorKeepsStoredKey(t *testing.T) {
	secret, stored := storedKeySecret(t)
	cs := fake.NewSimpleClientset(secret)
	startupFailFirstGets(cs, 1)

	got, created, err := loadOrCreateMiteSigningKey(startupCtx(t, 2*time.Second), cs, startupTestNS, startupOpts())
	if err != nil {
		t.Fatalf("loadOrCreateMiteSigningKey: %v", err)
	}
	if created {
		t.Fatal("created = true for a stored key")
	}
	if got.KID() != stored.KID() {
		t.Fatalf("KID = %s, want the stored KID %s", got.KID(), stored.KID())
	}
	startupAssertNoWrites(t, cs)
}

func TestLoadOrCreateMiteSigningKey_ReadFailureNeverOverwrites(t *testing.T) {
	secret, _ := storedKeySecret(t)
	cs := fake.NewSimpleClientset(secret)
	startupFailAllGets(cs)

	if _, _, err := loadOrCreateMiteSigningKey(startupCtx(t, 100*time.Millisecond), cs, startupTestNS, startupOpts()); err == nil {
		t.Fatal("expected an error when the read never succeeds")
	}
	startupAssertNoWrites(t, cs)
}

func TestLoadOrCreateMiteSigningKey_AlreadyExistsAdoptsStoredKey(t *testing.T) {
	competitor, competitorSigner := storedKeySecret(t)
	cs := fake.NewSimpleClientset()
	startupRaceCreate(cs, miteSigningKeySecretName, competitor.Data)

	got, created, err := loadOrCreateMiteSigningKey(startupCtx(t, 2*time.Second), cs, startupTestNS, startupOpts())
	if err != nil {
		t.Fatalf("loadOrCreateMiteSigningKey: %v", err)
	}
	if created {
		t.Fatal("created = true when another replica won the race")
	}
	if got.KID() != competitorSigner.KID() {
		t.Fatal("returned signer is not the competitor's key")
	}
}

func TestLoadOrCreateMiteSigningKey_CreatesOnFirstInstall(t *testing.T) {
	cs := fake.NewSimpleClientset()

	got, created, err := loadOrCreateMiteSigningKey(startupCtx(t, 2*time.Second), cs, startupTestNS, startupOpts())
	if err != nil {
		t.Fatalf("loadOrCreateMiteSigningKey: %v", err)
	}
	if !created {
		t.Fatal("created = false on first install")
	}
	stored, err := cs.CoreV1().Secrets(startupTestNS).Get(context.Background(), miteSigningKeySecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	reparsed, err := mitesrv.NewMiteTokenSignerFromPEM(stored.Data["private-key"])
	if err != nil {
		t.Fatalf("parse stored key: %v", err)
	}
	if reparsed.KID() != got.KID() {
		t.Fatal("KID of the stored PEM does not equal the returned KID")
	}
}
