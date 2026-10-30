package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
)

// heldItemLookup holds the catalog item lookup for the item named "held"
// until release closes, as a compose that clones a slow repository does.
// It answers every other lookup at once with an error, so the compose
// reports the item as missing and replies.
type heldItemLookup struct {
	started chan struct{}
	release chan struct{}
}

var errItemNotFound = errors.New("catalog item not found")

func (h *heldItemLookup) GetCatalogItemCRD(ctx context.Context, name, _ string) (*v1alpha1.CatalogItem, error) {
	if name == "held" {
		h.started <- struct{}{}
		select {
		case <-h.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, errItemNotFound
}

func composeRequest(t *testing.T, item string) []byte {
	t.Helper()
	spec := v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{
		{ItemRef: &v1alpha1.ComposedItemRef{Name: item, Namespace: "team-a"}},
	}}
	return mustJSON(t, bus.BundleComposeRequest{Namespace: "team-a", Spec: mustJSON(t, spec)})
}

// TestServeCommandResponders_HeldPreviewDoesNotBlockSecond pins that a
// bundle preview that waits in the composer does not hold the next bundle
// preview on the same subject. The command runner serves the subject in
// the operator-workers queue group on the leader only.
func TestServeCommandResponders_HeldPreviewDoesNotBlockSecond(t *testing.T) {
	lookup := &heldItemLookup{started: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(lookup.release)
	composer := bundle.NewComposer(lookup, nil, t.TempDir(), "")
	r := &operatorCommandRunner{
		cluster:    "core",
		configCrud: controller.NewConfigCRUD(nil, nil, composer, "", slog.Default()),
		logger:     slog.Default(),
	}
	subject := bus.OperatorBundlesSubject("core", "preview")
	client := startApproveRunner(t, func(ctx context.Context, conn *bus.Conn) error {
		stop, err := serveCommandResponders(conn, "operator-workers", r.commandResponders(ctx), r.logger)
		if err != nil {
			return err
		}
		<-ctx.Done()
		stop()
		return nil
	})
	free := composeRequest(t, "free")
	waitSubscribed(t, client, subject, free)

	held := requestInBackground(client, subject, composeRequest(t, "held"))
	select {
	case <-lookup.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the held preview did not reach the composer")
	}

	msg, err := client.Request(subject, free, time.Second)
	if err != nil {
		t.Fatalf("the second preview waited behind the held one: %v", err)
	}
	var resp bus.BundleComposeResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil || resp.Preview == nil {
		t.Fatalf("free reply = %s (decode error %v), want a preview", msg.Data, err)
	}

	lookup.release <- struct{}{}
	if got := <-held; got == nil {
		t.Fatal("the held preview got no reply after its release")
	}
}

// TestCommandResponders_OnlyComposeRowsAreConcurrent pins which subjects
// run more than one request at a time. A row may move to a concurrent
// limit only after its handler is known to be safe for concurrent calls.
// It also pins that the operator serves no bundles.validate subject: the
// BFF sends every compose on bundles.preview.
func TestCommandResponders_OnlyComposeRowsAreConcurrent(t *testing.T) {
	r := &operatorCommandRunner{cluster: "core", logger: slog.Default()}
	// concurrent maps each concurrent row to its limit and its subject.
	concurrent := map[string]struct {
		limit   int
		subject string
	}{
		"bundles.preview": {composeConcurrency, bus.OperatorBundlesSubject("core", "preview")},
		"cells.lint":      {cellLintConcurrency, bus.OperatorCellsSubject("core", "lint")},
	}
	seen := map[string]bool{}
	for _, cr := range r.commandResponders(context.Background()) {
		if seen[cr.subject] {
			t.Errorf("subject %s is served twice", cr.subject)
		}
		if cr.subject == bus.OperatorBundlesSubject("core", "validate") {
			t.Errorf("subject %s is served but has no sender", cr.subject)
		}
		seen[cr.subject] = true
		if want, ok := concurrent[cr.name]; ok {
			if cr.limit != want.limit {
				t.Errorf("%s limit = %d, want %d", cr.name, cr.limit, want.limit)
			}
			continue
		}
		if cr.limit != serialResponder {
			t.Errorf("%s limit = %d, want %d", cr.name, cr.limit, serialResponder)
		}
	}
	for name, want := range concurrent {
		if !seen[want.subject] {
			t.Errorf("no row serves %s", name)
		}
	}
}
