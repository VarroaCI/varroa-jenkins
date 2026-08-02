package controller

import (
	"log/slog"
	"os"
	"testing"
)

func TestResolveOperatorNamespace(t *testing.T) {
	// Save and restore env vars.
	origOp := os.Getenv("OPERATOR_NAMESPACE")
	origWatch := os.Getenv("WATCH_NAMESPACE")
	defer func() {
		os.Setenv("OPERATOR_NAMESPACE", origOp)
		os.Setenv("WATCH_NAMESPACE", origWatch)
	}()

	t.Run("OPERATOR_NAMESPACE wins", func(t *testing.T) {
		os.Setenv("OPERATOR_NAMESPACE", "op-ns")
		os.Setenv("WATCH_NAMESPACE", "watch-ns")
		got := ResolveOperatorNamespace(nil)
		if got != "op-ns" {
			t.Errorf("expected op-ns, got %q", got)
		}
	})

	t.Run("WATCH_NAMESPACE alias", func(t *testing.T) {
		os.Unsetenv("OPERATOR_NAMESPACE")
		os.Setenv("WATCH_NAMESPACE", "watch-ns")
		got := ResolveOperatorNamespace(slog.Default())
		if got != "watch-ns" {
			t.Errorf("expected watch-ns, got %q", got)
		}
	})

	t.Run("default fallback", func(t *testing.T) {
		os.Unsetenv("OPERATOR_NAMESPACE")
		os.Unsetenv("WATCH_NAMESPACE")
		got := ResolveOperatorNamespace(nil)
		if got != "varroa-system" {
			t.Errorf("expected varroa-system, got %q", got)
		}
	})

	t.Run("WATCH_NAMESPACE with nil logger does not panic", func(t *testing.T) {
		os.Unsetenv("OPERATOR_NAMESPACE")
		os.Setenv("WATCH_NAMESPACE", "watch-ns")
		got := ResolveOperatorNamespace(nil)
		if got != "watch-ns" {
			t.Errorf("expected watch-ns, got %q", got)
		}
	})
}
