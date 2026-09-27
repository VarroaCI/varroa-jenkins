package controller

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// captureDefaultWarnings sends the default slog logger to a buffer at WARN
// level for the duration of the test. ClientsetClient logs through the
// default logger, so the test must not run in parallel.
func captureDefaultWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestCreateServiceAccount_ExistingIsQuiet(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &ClientsetClient{clientset: cs}
	ctx := context.Background()
	if err := c.CreateServiceAccount(ctx, "varroa-agent-alpha", "cells"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	logs := captureDefaultWarnings(t)
	if err := c.CreateServiceAccount(ctx, "varroa-agent-alpha", "cells"); err != nil {
		t.Fatalf("second create: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("second create logged a warning: %s", logs.String())
	}
	if _, err := cs.CoreV1().ServiceAccounts("cells").Get(ctx, "varroa-agent-alpha", metav1.GetOptions{}); err != nil {
		t.Fatalf("ServiceAccount missing after create: %v", err)
	}
}

func TestCreateServiceAccountTokenSecret_ExistingIsQuiet(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &ClientsetClient{clientset: cs}
	ctx := context.Background()
	if err := c.CreateServiceAccountTokenSecret(ctx, "varroa-agent-alpha-token", "cells", "varroa-agent-alpha"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	logs := captureDefaultWarnings(t)
	if err := c.CreateServiceAccountTokenSecret(ctx, "varroa-agent-alpha-token", "cells", "varroa-agent-alpha"); err != nil {
		t.Fatalf("second create: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("second create logged a warning: %s", logs.String())
	}
	creates := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "secrets" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("want 1 Secret create across both calls, got %d", creates)
	}
}

func TestCreateServiceAccountTokenSecret_ForeignSecretWarns(t *testing.T) {
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-agent-alpha-token", Namespace: "cells"},
		Type:       corev1.SecretTypeOpaque,
	}
	cs := fake.NewSimpleClientset(foreign)
	c := &ClientsetClient{clientset: cs}
	logs := captureDefaultWarnings(t)
	if err := c.CreateServiceAccountTokenSecret(context.Background(), "varroa-agent-alpha-token", "cells", "varroa-agent-alpha"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(logs.String(), "not a token for the service account") || !strings.Contains(logs.String(), "varroa-agent-alpha-token") {
		t.Fatalf("a Secret that is not a token for the ServiceAccount must log a warning, got %q", logs.String())
	}
}

func TestCreateServiceAccountTokenSecret_LostCreateRaceChecksWinner(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		foreign := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "varroa-agent-alpha-token", Namespace: "cells"},
			Type:       corev1.SecretTypeOpaque,
		}
		if err := cs.Tracker().Add(foreign); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewAlreadyExists(corev1.Resource("secrets"), foreign.Name)
	})
	c := &ClientsetClient{clientset: cs}
	logs := captureDefaultWarnings(t)
	if err := c.CreateServiceAccountTokenSecret(context.Background(), "varroa-agent-alpha-token", "cells", "varroa-agent-alpha"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(logs.String(), "not a token for the service account") {
		t.Fatalf("a foreign Secret that wins the create race must log a warning, got %q", logs.String())
	}
}
