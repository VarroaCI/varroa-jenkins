package mite

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/grpc"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// openHelloStream opens a CommandStream on gconn and sends its hello for
// name/ns.
func openHelloStream(t *testing.T, gconn *grpc.ClientConn, name, ns string) mitev1.Mite_CommandStreamClient {
	t.Helper()
	stream, err := mitev1.NewMiteClient(gconn).CommandStream(context.Background())
	if err != nil {
		t.Fatalf("command stream: %v", err)
	}
	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_Hello{
			Hello: &mitev1.Hello{ControllerName: name, Namespace: ns, Version: "test-v1"},
		},
	}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	return stream
}

// sendStartedEvent sends one STARTED build event of runID on stream.
func sendStartedEvent(t *testing.T, stream mitev1.Mite_CommandStreamClient, runID, jobPath string) {
	t.Helper()
	if err := stream.Send(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_BuildEvent{
			BuildEvent: &mitev1.BuildEvent{
				Phase:           phaseStarted,
				RunId:           runID,
				IntentKey:       "manual:" + runID,
				JobPath:         jobPath,
				BuildNumber:     1,
				Seq:             1,
				EmittedAtMillis: time.Now().UnixMilli(),
			},
		},
	}); err != nil {
		t.Fatalf("send build event: %v", err)
	}
}

// awaitEnvelopeInstance waits for the envelope of runID on envCh and
// answers its cell instance.
func awaitEnvelopeInstance(t *testing.T, envCh <-chan []byte, runID string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case data := <-envCh:
			var env ingestEnvelope
			if err := json.Unmarshal(data, &env); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			if env.RunID == runID {
				return env.CellInstance
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the envelope of %s", runID)
		}
	}
}

// TestGatewayIntegration_BuildEventCarriesItsOwnStreamInstance proves that
// each build event carries the instance of the stream that sent it. Two
// pods of one controller reach one gateway replica. Pod B holds the
// current stream. Pod A then registers on the same replica, which
// overwrites the per-controller instance entry. B's next event must still
// carry B, because the record store drops a running-build fact from an
// instance that does not own the run. A's own stream then carries A.
func TestGatewayIntegration_BuildEventCarriesItsOwnStreamInstance(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, lis, certAuth := startGatewayWithCA(t, natsSrv.ClientURL())
	if err := busConn.EnsureStream(bus.BuildsStreamConfig(bus.BuildsStreamName)); err != nil {
		t.Fatalf("ensure builds stream: %v", err)
	}
	envCh := make(chan []byte, 16)
	sub, err := busConn.NATSConn().Subscribe(bus.BuildsWildcard, func(msg *nats.Msg) {
		envCh <- msg.Data
	})
	if err != nil {
		t.Fatalf("subscribe builds: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	addr := lis.Addr().String()
	name, ns := "twinctl", "twinns"
	jobPath := ns + "/" + name
	const podA, podB = "pod-a", "pod-b"

	connB := dialWithMiteCertCellInstance(t, addr, certAuth, name, ns, podB)
	streamB := openHelloStream(t, connB, name, ns)
	sendStartedEvent(t, streamB, "run-b-1", jobPath)
	if got := awaitEnvelopeInstance(t, envCh, "run-b-1"); got != podB {
		t.Fatalf("first event of stream B carries %q, want %q", got, podB)
	}

	connA := dialWithMiteCertCellInstance(t, addr, certAuth, name, ns, podA)
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if _, err := mitev1.NewMiteClient(connA).Register(context.Background(), &mitev1.RegisterRequest{
		ControllerName: name,
		Namespace:      ns,
		PublicKey:      []byte(pub),
		CellInstance:   podA,
	}); err != nil {
		t.Fatalf("register pod A: %v", err)
	}

	sendStartedEvent(t, streamB, "run-b-2", jobPath)
	if got := awaitEnvelopeInstance(t, envCh, "run-b-2"); got != podB {
		t.Fatalf("event of stream B after pod A registered carries %q, want %q", got, podB)
	}

	streamA := openHelloStream(t, connA, name, ns)
	sendStartedEvent(t, streamA, "run-a-1", jobPath)
	if got := awaitEnvelopeInstance(t, envCh, "run-a-1"); got != podA {
		t.Fatalf("event of stream A carries %q, want %q", got, podA)
	}
}

// TestOnStreamBuildEventCompletedDecidesTheOwnerFromTheStreamInstance
// proves that a COMPLETED event decides its owner from the instance of
// the stream that sent it, not from the per-controller entry. Another
// pod of the same controller can overwrite that entry while the stream
// is still the current one.
func TestOnStreamBuildEventCompletedDecidesTheOwnerFromTheStreamInstance(t *testing.T) {
	cases := []struct {
		name           string
		streamInstance string
		mapInstance    string
		wantStages     int
	}{
		{name: "owner stream while the entry names another pod", streamInstance: "inst2", mapInstance: "inst-zombie", wantStages: 1},
		{name: "stale stream while the entry names the owner", streamInstance: "inst-zombie", mapInstance: "inst2", wantStages: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := newFakeOwnerRequester()
			runID := newTestRunID()
			owner.setAnswer(runID, ownerLookupResponse{Found: true, Cluster: "core", Namespace: "ns1", Cell: "cell2", Instance: "inst2"})
			h, _ := newCompletedOwnerTestHandler(t, owner, &recordingSender{})
			h.RecordCellInstance("cell2", "ns1", tc.mapInstance)
			h.logFinalizeWait = time.Hour

			evt := completedBuildEvent(runID, time.Now())
			evt.Stages = []*mitev1.BuildStage{{StageId: "s1", Name: "build", NodeType: "stage", Status: "SUCCESS"}}
			if err := h.OnStreamBuildEvent("cell2", "ns1", tc.streamInstance, 0, evt); err != nil {
				t.Fatalf("OnStreamBuildEvent: %v", err)
			}

			wantOwner := tc.wantStages == 1
			if _, exists := h.lookupRunState(runID); exists != wantOwner {
				t.Fatalf("run state exists = %v, want %v", exists, wantOwner)
			}
			stages := 0
			for _, env := range queuedEnvelopes(t, h) {
				if env.CellInstance != tc.streamInstance {
					t.Errorf("%s envelope carries %q, want %q", env.Kind, env.CellInstance, tc.streamInstance)
				}
				if env.Kind == kindRunStage {
					stages++
				}
			}
			if stages != tc.wantStages {
				t.Fatalf("run.stage envelopes = %d, want %d", stages, tc.wantStages)
			}
		})
	}
}

// TestOnStreamBuildEventWithNoStreamInstanceKeepsTheRegisteredOne proves
// that a stream whose certificate names no instance still stamps the
// instance the controller last registered, as before.
func TestOnStreamBuildEventWithNoStreamInstanceKeepsTheRegisteredOne(t *testing.T) {
	h, _ := newCompletedOwnerTestHandler(t, newFakeOwnerRequester(), &recordingSender{})

	evt := &mitev1.BuildEvent{Phase: phaseStarted, RunId: newTestRunID(), Seq: 1, JobPath: "tenant-a/build"}
	if err := h.OnStreamBuildEvent("cell2", "ns1", "", 0, evt); err != nil {
		t.Fatalf("OnStreamBuildEvent: %v", err)
	}

	envs := queuedEnvelopes(t, h)
	if len(envs) != 1 || envs[0].CellInstance != "inst2" {
		t.Fatalf("envelopes = %+v, want one that carries inst2", envs)
	}
}
