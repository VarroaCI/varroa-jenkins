package transport

import (
	"context"
	"fmt"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// BusReadModelTransport wraps a BusReadModel to implement Transport for
// read-only BFF use. Read methods are delegated to the underlying
// BusReadModel. Send, DrainResults, and ClearDesired return errors.
// SendImperative delegates to PublishSafeRestart for publish-only delivery.
type BusReadModelTransport struct {
	model *BusReadModel
}

// NewBusReadModelTransport creates a read-only transport adapter that delegates
// reads to the underlying BusReadModel and provides a publish-only SendImperative.
func NewBusReadModelTransport(model *BusReadModel) *BusReadModelTransport {
	return &BusReadModelTransport{model: model}
}

// Send returns an error; BusReadModelTransport is read-only.
func (t *BusReadModelTransport) Send(_ context.Context, _, _ string, _ *mitev1.OperatorMessage) error {
	return fmt.Errorf("BusReadModelTransport does not support Send")
}

// SendImperative delegates to the publish-only PublishSafeRestart method.
func (t *BusReadModelTransport) SendImperative(ctx context.Context, ns, name string, cmd *mitev1.ImperativeCommand) error {
	return t.model.PublishSafeRestart(ctx, ns, name, cmd)
}

// DrainResults returns nil; BusReadModelTransport does not collect command results.
func (t *BusReadModelTransport) DrainResults(_, _ string) []*mitev1.CommandResult {
	return nil
}

// Snapshot delegates to the underlying BusReadModel.
func (t *BusReadModelTransport) Snapshot(ns, name string) *mitev1.StateSnapshot {
	return t.model.Snapshot(ns, name)
}

// Health delegates to the underlying BusReadModel.
func (t *BusReadModelTransport) Health(ns, name string) string {
	return t.model.Health(ns, name)
}

// Connected delegates to the underlying BusReadModel.
func (t *BusReadModelTransport) Connected(ns, name string) bool {
	return t.model.Connected(ns, name)
}

// Info delegates to the underlying BusReadModel.
func (t *BusReadModelTransport) Info(ns, name string) (string, time.Time, time.Time, bool) {
	return t.model.Info(ns, name)
}

// IdleGauges delegates to the underlying BusReadModel.
func (t *BusReadModelTransport) IdleGauges(ns, name string) (*mitev1.IdleGauges, time.Time, bool) {
	return t.model.IdleGauges(ns, name)
}

// ConnEpoch returns zero values; BusReadModelTransport does not track connection epochs.
func (t *BusReadModelTransport) ConnEpoch(_, _ string) (int64, bool) {
	return 0, false
}

// ClearDesired returns an error; BusReadModelTransport is read-only.
func (t *BusReadModelTransport) ClearDesired(_ context.Context, _, _ string) error {
	return fmt.Errorf("BusReadModelTransport does not support ClearDesired")
}

// ObservabilityReport delegates to the underlying BusReadModel.
func (t *BusReadModelTransport) ObservabilityReport(ns, name string) *mitev1.ObservabilityReport {
	return t.model.ObservabilityReport(ns, name)
}

// FetchLastApplied delegates to the underlying BusReadModel which uses the
// NATS content-fetch bridge to request the mite's last-applied content.
func (t *BusReadModelTransport) FetchLastApplied(ctx context.Context, ns, name string) (*mitev1.ContentResponse, error) {
	return t.model.FetchLastAppliedContent(ctx, ns, name)
}

// PluginInventory delegates to the underlying BusReadModel.
func (t *BusReadModelTransport) PluginInventory(ns, name string) *mitev1.PluginInventory {
	return t.model.PluginInventory(ns, name)
}

// InstalledPluginsHash delegates to the underlying BusReadModel.
func (t *BusReadModelTransport) InstalledPluginsHash(ns, name string) (string, bool) {
	return t.model.InstalledPluginsHash(ns, name)
}

// PluginClassification delegates to the underlying BusReadModel.
func (t *BusReadModelTransport) PluginClassification(ns, name string) (*ClassifiedInventory, bool) {
	return t.model.PluginClassification(ns, name)
}

// PutPluginClassification returns a read-only error; the BFF must never write.
func (t *BusReadModelTransport) PutPluginClassification(_ context.Context, _, _ string, _ *ClassifiedInventory) error {
	return fmt.Errorf("BusReadModelTransport does not support PutPluginClassification")
}
