package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/forgeops"
	"github.com/varroaci/varroa-jenkins/internal/controller/sharding"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// stampOps answers every forge verb with the replica id, so a test can tell
// which replica replied.
type stampOps struct {
	id       string
	deadline *time.Time
}

func (s stampOps) Check(ctx context.Context, _ forgeapi.CheckRequest) forgeapi.CheckReply {
	if s.deadline != nil {
		if d, ok := ctx.Deadline(); ok {
			*s.deadline = d
		}
	}
	return forgeapi.CheckReply{Status: forgeapi.Status{OK: true}, CredentialID: s.id}
}

func (s stampOps) Read(_ context.Context, _ forgeapi.ReadRequest) forgeapi.ReadReply {
	return forgeapi.ReadReply{Status: forgeapi.Status{OK: true}, Revision: s.id}
}

func (s stampOps) Issue(_ context.Context, _ forgeapi.IssueRequest, _ string) forgeapi.IssueReply {
	return forgeapi.IssueReply{Status: forgeapi.Status{OK: true}, Number: 1, URL: s.id}
}

func issueBody(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(forgeapi.IssueRequest{Team: "payments", Title: "t", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestHandleForgeIssue_NotShardOwnerSendsNoReply checks that only the shard
// owner of the Team opens the issue and answers.
func TestHandleForgeIssue_NotShardOwnerSendsNoReply(t *testing.T) {
	ring := sharding.NewRing(8)
	got := handleForgeIssue(context.Background(), slog.Default(), stampOps{id: "x"}, ownerEnter(map[int]bool{}, ring), "varroa-system", issueBody(t))
	if got != nil {
		t.Fatalf("non-owner replied: %s", got)
	}
}

// ownerEnter holds only the shards in held.
func ownerEnter(held map[int]bool, ring *sharding.Ring) shardEnterer {
	return func(ctx context.Context, key string) (context.Context, func(), bool) {
		if !held[ring.ShardFor(key)] {
			return ctx, func() {}, false
		}
		return ctx, func() {}, true
	}
}

func checkBody(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(forgeapi.CheckRequest{Team: "payments", Repo: "https://git.example.org/team/app"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readBody(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(forgeapi.ReadRequest{Team: "payments", Repo: "https://git.example.org/team/app", Path: "README.md"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHandleForgeCheck_NonOwnerSendsNoReply(t *testing.T) {
	ring := sharding.NewRing(8)
	got := handleForgeCheck(context.Background(), slog.Default(), stampOps{id: "x"}, ownerEnter(map[int]bool{}, ring), checkBody(t))
	if got != nil {
		t.Fatalf("non-owner replied: %s", got)
	}
}

func TestHandleForgeRead_NonOwnerSendsNoReply(t *testing.T) {
	ring := sharding.NewRing(8)
	got := handleForgeRead(context.Background(), slog.Default(), stampOps{id: "x"}, ownerEnter(map[int]bool{}, ring), readBody(t))
	if got != nil {
		t.Fatalf("non-owner replied: %s", got)
	}
}

func TestHandleForge_OwnerReplies(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	got := handleForgeCheck(context.Background(), slog.Default(), stampOps{id: "owner"}, ownerEnter(held, ring), checkBody(t))
	var rep forgeapi.CheckReply
	if err := json.Unmarshal(got, &rep); err != nil {
		t.Fatalf("decode %q: %v", got, err)
	}
	if !rep.OK || rep.CredentialID != "owner" {
		t.Fatalf("reply %+v", rep)
	}
	got = handleForgeRead(context.Background(), slog.Default(), stampOps{id: "owner"}, ownerEnter(held, ring), readBody(t))
	var rr forgeapi.ReadReply
	if err := json.Unmarshal(got, &rr); err != nil || rr.Revision != "owner" {
		t.Fatalf("read reply %q: %v", got, err)
	}
}

func TestHandleForge_DecodeErrorRepliesInvalid(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("")): true}
	got := handleForgeCheck(context.Background(), slog.Default(), stampOps{id: "x"}, ownerEnter(held, ring), []byte("{not json"))
	var rep forgeapi.CheckReply
	if err := json.Unmarshal(got, &rep); err != nil || rep.Code != forgeapi.CodeInvalidRequest {
		t.Fatalf("reply %q: %v", got, err)
	}
}

func TestHandleForge_SetsDeadline(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	var seen time.Time
	before := time.Now()
	handleForgeCheck(context.Background(), slog.Default(), stampOps{deadline: &seen}, ownerEnter(held, ring), checkBody(t))
	if seen.IsZero() {
		t.Fatal("no deadline on the forge call")
	}
	if d := seen.Sub(before); d < forgeRequestTimeout || d > forgeRequestTimeout+2*time.Second {
		t.Fatalf("deadline %v after the call start, want about %v", d, forgeRequestTimeout)
	}
}

// startForgeReplica runs one forge runner for verb on a replica that holds the
// shards in held. The runner stops when the test ends.
func startForgeReplica(t *testing.T, url, id, verb string, ring *sharding.Ring, held map[int]bool) {
	t.Helper()
	conn, err := bus.Connect(url)
	if err != nil {
		t.Fatalf("connect replica %s: %v", id, err)
	}
	t.Cleanup(conn.Close)
	set := sharding.NewShardSet()
	for shard := range held {
		set.Add(shard)
	}
	enter := func(ctx context.Context, key string) (context.Context, func(), bool) {
		return set.Enter(ctx, ring.ShardFor(key))
	}
	logger := slog.Default()
	r := &forgeRunner{
		busConn: conn,
		cluster: "core",
		verb:    verb,
		limit:   forgeConcurrency,
		logger:  logger,
		handle: func(ctx context.Context, data []byte) []byte {
			if verb == forgeapi.VerbCheck {
				return handleForgeCheck(ctx, logger, stampOps{id: id}, enter, data)
			}
			return handleForgeRead(ctx, logger, stampOps{id: id}, enter, data)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// TestForgeRunners_OnlyOwnerReplies runs two replicas and checks that exactly
// one reply comes back for each request, from the shard owner.
func TestForgeRunners_OnlyOwnerReplies(t *testing.T) {
	for _, verb := range []string{forgeapi.VerbCheck, forgeapi.VerbRead} {
		t.Run(verb, func(t *testing.T) {
			s := startLedgerActionNATS(t)
			url := s.ClientURL()
			ring := sharding.NewRing(8)
			own := ring.ShardFor(forgeops.ShardKey("payments"))
			aHeld := map[int]bool{own: true}
			bHeld := map[int]bool{}
			for i := 0; i < 8; i++ {
				if i != own {
					bHeld[i] = true
				}
			}
			startForgeReplica(t, url, "A", verb, ring, aHeld)
			startForgeReplica(t, url, "B", verb, ring, bHeld)

			client, err := nats.Connect(url)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.Close)
			subject := bus.OperatorForgeSubject("core", verb)
			body := checkBody(t)
			if verb == forgeapi.VerbRead {
				body = readBody(t)
			}

			// Wait for the subscriptions with a warm-up request on its own inbox.
			warm := client.NewRespInbox()
			warmSub, err := client.SubscribeSync(warm)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if err := client.PublishRequest(subject, warm, body); err != nil {
					t.Fatal(err)
				}
				if _, err := warmSub.NextMsg(200 * time.Millisecond); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("no forge %s runner answered", verb)
				}
			}

			inbox := client.NewRespInbox()
			sub, err := client.SubscribeSync(inbox)
			if err != nil {
				t.Fatal(err)
			}
			if err := client.PublishRequest(subject, inbox, body); err != nil {
				t.Fatal(err)
			}
			msg, err := sub.NextMsg(2 * time.Second)
			if err != nil {
				t.Fatalf("no reply: %v", err)
			}
			if got := stampOf(t, verb, msg.Data); got != "A" {
				t.Fatalf("reply from %q, want the owner A", got)
			}
			if _, err := sub.NextMsg(500 * time.Millisecond); err == nil {
				t.Fatal("a second reply arrived")
			}
		})
	}
}

// stampOf returns the replica id that a reply carries.
func stampOf(t *testing.T, verb string, data []byte) string {
	t.Helper()
	if verb == forgeapi.VerbCheck {
		var rep forgeapi.CheckReply
		if err := json.Unmarshal(data, &rep); err != nil {
			t.Fatal(err)
		}
		return rep.CredentialID
	}
	var rep forgeapi.ReadReply
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	return rep.Revision
}
