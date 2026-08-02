package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func TestPrimeWakeRootDomain(t *testing.T) {
	want := &v1alpha1.ProvisioningDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
		Spec:       v1alpha1.ProvisioningDefaultsSpec{RootDomain: "jenkins.example.com"},
	}
	store := crdstore.NewFake()
	if err := store.Seed(want); err != nil {
		t.Fatal(err)
	}
	setter := &wakeDefaultsSetter{}
	primeWakeRootDomain(context.Background(), store, setter, slog.Default())
	if setter.defaults == nil {
		t.Fatal("expected defaults to be set")
	}
	if setter.defaults.Spec.RootDomain != "jenkins.example.com" {
		t.Fatalf("root domain = %q, want jenkins.example.com", setter.defaults.Spec.RootDomain)
	}
}

func TestPrimeWakeRootDomainFailureIsNonBlocking(t *testing.T) {
	var logs bytes.Buffer
	store := crdstore.NewFake()
	// Inject a transient (non-NotFound) API failure — the wake server must
	// come up regardless, with only a warning logged.
	pdGVR, err := crdstore.GVRFor[v1alpha1.ProvisioningDefaults]()
	if err != nil {
		t.Fatal(err)
	}
	store.FailAlways("get", pdGVR, errors.New("apiserver timeout"))
	setter := &wakeDefaultsSetter{}
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	primeWakeRootDomain(context.Background(), store, setter, logger)

	if setter.defaults != nil {
		t.Fatalf("unexpected defaults: %#v", setter.defaults)
	}
	if !strings.Contains(logs.String(), "initial provisioning defaults fetch for wake server") {
		t.Fatalf("warning not logged: %s", logs.String())
	}
}

type wakeDefaultsSetter struct {
	defaults *v1alpha1.ProvisioningDefaults
}

func (s *wakeDefaultsSetter) SetProvisioningDefaults(defaults *v1alpha1.ProvisioningDefaults) {
	s.defaults = defaults
}
