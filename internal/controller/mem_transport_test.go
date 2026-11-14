package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// memTransport is the in-memory transport.Transport of the reconciler tests.
// A test calls connect to write the presence record of a mite and heartbeat
// to store its snapshot. memTransport holds no mite stream. Send and
// SendImperative always return a plain error, never
// transport.ErrMiteUnreachable, so a push marks the reconcile blocked.
type memTransport struct {
	mu              sync.Mutex
	mites           map[string]*memMite // keyed "ns/name"
	nextEpoch       int64
	classifications map[string]*transport.ClassifiedInventory // keyed "ns/name"
}

// memMite is the presence record of one mite in a memTransport.
type memMite struct {
	info     transport.MiteInfo
	epoch    int64
	snapshot *mitev1.StateSnapshot
}

var _ transport.Transport = (*memTransport)(nil)

func newMemTransport() *memTransport {
	return &memTransport{
		mites:           make(map[string]*memMite),
		classifications: make(map[string]*transport.ClassifiedInventory),
	}
}

// connect writes a new presence record for ns/name with the next epoch. The
// record replaces an earlier record and its snapshot.
func (m *memTransport) connect(name, ns string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextEpoch++
	m.mites[ns+"/"+name] = &memMite{
		info:  transport.MiteInfo{Version: "v1.0", LastHeartbeat: time.Now(), CertExpiry: time.Now().Add(24 * time.Hour)},
		epoch: m.nextEpoch,
	}
}

// heartbeat refreshes the heartbeat time of ns/name and stores snap. It does
// nothing when ns/name has no presence record.
func (m *memTransport) heartbeat(name, ns string, snap *mitev1.StateSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.mites[ns+"/"+name]
	if !ok {
		return
	}
	rec.info.LastHeartbeat = time.Now()
	if snap != nil {
		rec.snapshot = snap
	}
}

func (m *memTransport) record(ns, name string) (memMite, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.mites[ns+"/"+name]
	if !ok {
		return memMite{}, false
	}
	return *rec, true
}

func (m *memTransport) Send(_ context.Context, ns, name string, _ *mitev1.OperatorMessage) error {
	return fmt.Errorf("mite %s/%s: the test transport holds no stream", ns, name)
}

func (m *memTransport) SendImperative(_ context.Context, ns, name string, _ *mitev1.ImperativeCommand) error {
	return fmt.Errorf("mite %s/%s: the test transport holds no stream", ns, name)
}

func (m *memTransport) DrainResults(_, _ string) []*mitev1.CommandResult { return nil }

func (m *memTransport) Snapshot(ns, name string) *mitev1.StateSnapshot {
	rec, _ := m.record(ns, name)
	return rec.snapshot
}

func (m *memTransport) Health(ns, name string) string {
	snap := m.Snapshot(ns, name)
	switch {
	case snap == nil:
		return "unreachable"
	case snap.JenkinsHealth == "":
		return "unknown"
	default:
		return snap.JenkinsHealth
	}
}

func (m *memTransport) Connected(_, _ string) bool { return false }

func (m *memTransport) Info(ns, name string) (transport.MiteInfo, bool, error) {
	rec, ok := m.record(ns, name)
	return rec.info, ok, nil
}

func (m *memTransport) IdleGauges(_, _ string) (*mitev1.IdleGauges, time.Time, bool) {
	return nil, time.Time{}, false
}

func (m *memTransport) StreamDegraded(_, _ string) (string, bool) { return "", false }

func (m *memTransport) ConnEpoch(ns, name string) (int64, bool) {
	rec, ok := m.record(ns, name)
	return rec.epoch, ok
}

func (m *memTransport) ObservabilityReport(_, _ string) *mitev1.ObservabilityReport { return nil }

func (m *memTransport) EnsureInbound(_, _ string) {}

func (m *memTransport) FetchLastApplied(_ context.Context, _, _ string) (*mitev1.ContentResponse, error) {
	return nil, transport.ErrContentUnavailable
}

func (m *memTransport) PluginInventory(_, _ string) *mitev1.PluginInventory { return nil }

func (m *memTransport) InstalledPluginsHash(_, _ string) (string, bool) { return "", false }

func (m *memTransport) PluginClassification(ns, name string) (*transport.ClassifiedInventory, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.classifications[ns+"/"+name]
	return c, ok
}

func (m *memTransport) PutPluginClassification(_ context.Context, ns, name string, c *transport.ClassifiedInventory) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.classifications[ns+"/"+name] = c
	return nil
}

func (m *memTransport) Forget(_, _ string) {}

func (m *memTransport) ForgetMatching(func(ns, name string) bool) {}
