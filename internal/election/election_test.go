package election

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

const testLease = "test-lease"

func testLock(cs *fake.Clientset) *resourcelock.LeaseLock {
	return &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: testLease, Namespace: "ns"},
		Client:     cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: "replica-a"},
	}
}

func TestTimingIsValid(t *testing.T) {
	cs := fake.NewClientset()
	_, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          testLock(cs),
		Name:          testLease,
		LeaseDuration: LeaseDuration,
		RenewDeadline: RenewDeadline,
		RetryPeriod:   RetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(context.Context) {},
			OnStoppedLeading: func() {},
		},
	})
	if err != nil {
		t.Fatalf("NewLeaderElector: %v", err)
	}
}

func TestTimingKeepsClusterHeartbeatFresh(t *testing.T) {
	// A crashed leader's entry is refreshed by the next leader after at most one
	// heartbeat interval, one lease duration, and two jittered retry waits.
	worst := bus.ClusterHeartbeatInterval + LeaseDuration +
		2*time.Duration(float64(RetryPeriod)*(1+leaderelection.JitterFactor))
	if worst >= bus.ClusterEntryTTL {
		t.Fatalf("worst heartbeat gap %v must stay under ClusterEntryTTL %v", worst, bus.ClusterEntryTTL)
	}
}

func TestTimingOutlastsRetransmitStall(t *testing.T) {
	// A link loss of up to about 25 seconds stalls API requests until the Linux
	// retransmit timer fires. The timer starts at 200 ms and doubles six times,
	// so the last resend falls about 25.4 seconds after the loss.
	const stall = 25400 * time.Millisecond
	if RenewDeadline <= stall {
		t.Fatalf("RenewDeadline %v must exceed the retransmit stall %v", RenewDeadline, stall)
	}
}

func TestTimingStopsLeaderBeforeTakeover(t *testing.T) {
	if RenewDeadline+RetryPeriod >= LeaseDuration {
		t.Fatalf("RenewDeadline+RetryPeriod (%v) must be below LeaseDuration (%v)",
			RenewDeadline+RetryPeriod, LeaseDuration)
	}
}

func TestRunReturnsErrorForInvalidTiming(t *testing.T) {
	cs := fake.NewClientset()
	err := run(context.Background(), testLock(cs), testLease,
		timing{lease: time.Second, renew: 2 * time.Second, retry: 100 * time.Millisecond},
		func(context.Context) {})
	if err == nil {
		t.Fatal("run accepted RenewDeadline above LeaseDuration")
	}
}

var fastTiming = timing{lease: 400 * time.Millisecond, renew: 300 * time.Millisecond, retry: 50 * time.Millisecond}

func TestRunCampaignsAgainAfterLostLease(t *testing.T) {
	cs := fake.NewClientset()
	var failing atomic.Bool
	fail := func(action k8stesting.Action) (bool, runtime.Object, error) {
		if failing.Load() {
			return true, nil, errors.New("api server unavailable")
		}
		return false, nil, nil
	}
	cs.PrependReactor("get", "leases", fail)
	cs.PrependReactor("update", "leases", fail)

	starts := make(chan struct{}, 4)
	stops := make(chan struct{}, 4)
	var active atomic.Int32
	lead := func(c context.Context) {
		if n := active.Add(1); n > 1 {
			t.Error("lead runs twice at once")
		}
		starts <- struct{}{}
		<-c.Done()
		active.Add(-1)
		stops <- struct{}{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, testLock(cs), testLease, fastTiming, lead) }()

	wait := func(ch chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	wait(starts, "first start")
	failing.Store(true)
	wait(stops, "lost lease")
	failing.Store(false)
	wait(starts, "second start after recampaign")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

func TestRunReleasesLeaseWhenContextEnds(t *testing.T) {
	cs := fake.NewClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var once sync.Once
	leading := make(chan struct{})
	lead := func(c context.Context) {
		once.Do(func() { close(leading) })
		<-c.Done()
	}

	done := make(chan error, 1)
	go func() { done <- run(ctx, testLock(cs), testLease, fastTiming, lead) }()

	select {
	case <-leading:
	case <-time.After(5 * time.Second):
		t.Fatal("never started leading")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}

	lease, err := cs.CoordinationV1().Leases("ns").Get(context.Background(), testLease, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	var holder string
	if lease.Spec.HolderIdentity != nil {
		holder = *lease.Spec.HolderIdentity
	}
	if holder != "" {
		t.Fatalf("lease holder = %q after release, want empty", holder)
	}
}
