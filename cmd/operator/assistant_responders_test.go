package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
)

// TestRespondersIncludeAssistantOnlyWithFlag pins that the AssistantConfig write
// subject is served only when the operator runs with -assistant.
func TestRespondersIncludeAssistantOnlyWithFlag(t *testing.T) {
	subject := bus.OperatorAssistantConfigSubject("core", "update")
	has := func(r *operatorCommandRunner) bool {
		for _, c := range r.commandResponders(context.Background()) {
			if c.subject == subject {
				return true
			}
		}
		return false
	}
	base := func(assistant bool) *operatorCommandRunner {
		return &operatorCommandRunner{
			cluster:    "core",
			configCrud: controller.NewConfigCRUD(nil, nil, nil, "", slog.Default()),
			assistant:  assistant,
			logger:     slog.Default(),
		}
	}
	if has(base(false)) {
		t.Fatal("assistantconfig.update served without -assistant")
	}
	if !has(base(true)) {
		t.Fatal("assistantconfig.update not served with -assistant")
	}
}
