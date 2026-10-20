package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/forgeops"
	"github.com/varroaci/varroa-jenkins/internal/controller/sharding"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/keylock"
)

// stampOps answers every forge verb with the replica id, so a test can tell
// which replica replied.
type stampOps struct {
	id       string
	deadline *time.Time
	// calls counts the PullRequest and Issue calls when it is not nil.
	calls *atomic.Int32
	// block holds PullRequest and Issue until it is closed, when it is not nil.
	block chan struct{}
	// blockTeam limits block to the pull requests of one Team. An empty
	// blockTeam holds every pull request.
	blockTeam string
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

func (s stampOps) Issue(ctx context.Context, _ forgeapi.IssueRequest, _ string) forgeapi.IssueReply {
	if s.calls != nil {
		s.calls.Add(1)
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
		}
	}
	if s.deadline != nil {
		if d, ok := ctx.Deadline(); ok {
			*s.deadline = d
		}
	}
	return forgeapi.IssueReply{Status: forgeapi.Status{OK: true}, Number: 1, URL: s.id}
}

func (s stampOps) PullRequest(ctx context.Context, req forgeapi.PullRequestRequest) forgeapi.PullRequestReply {
	if s.calls != nil {
		s.calls.Add(1)
	}
	if s.block != nil && (s.blockTeam == "" || req.Team == s.blockTeam) {
		select {
		case <-s.block:
		case <-ctx.Done():
		}
	}
	if s.deadline != nil {
		if d, ok := ctx.Deadline(); ok {
			*s.deadline = d
		}
	}
	return forgeapi.PullRequestReply{Status: forgeapi.Status{OK: true}, Number: 1, URL: s.id}
}

func prBody(t *testing.T) []byte {
	t.Helper()
	return prBodyDeadline(t, time.Now().Add(forgeapi.PullRequestTimeout))
}

// prBodyDeadline returns a pull-request request for the payments Team that
// carries the deadline d.
func prBodyDeadline(t *testing.T, d time.Time) []byte {
	t.Helper()
	return prBodyTeam(t, "payments", d)
}

// prBodyTeam returns a pull-request request for team that carries the deadline d.
func prBodyTeam(t *testing.T, team string, d time.Time) []byte {
	t.Helper()
	b, err := json.Marshal(forgeapi.PullRequestRequest{Team: team, Repo: "https://git.example.org/a/b", Branch: "varroa/propose-pipeline", Title: "t", Body: "b", CommitMessage: "c", Files: []forgeapi.File{{Path: "Jenkinsfile", Content: "x"}}, Deadline: d})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestHandleForgePullRequest_NonOwnerSendsNoReply checks that only the shard
// owner of the Team opens the pull request and answers.
func TestHandleForgePullRequest_NonOwnerSendsNoReply(t *testing.T) {
	ring := sharding.NewRing(8)
	got := handleForgePullRequest(context.Background(), slog.Default(), stampOps{id: "x"}, ownerEnter(map[int]bool{}, ring), keylock.New(), prBody(t))
	if got != nil {
		t.Fatalf("non-owner replied: %s", got)
	}
}

// TestHandleForgePullRequest_DeadlineLeavesReserve checks that the forge call
// ends the reserve before the requester's deadline.
func TestHandleForgePullRequest_DeadlineLeavesReserve(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	var seen time.Time
	reqDeadline := time.Now().Add(forgeapi.PullRequestTimeout)
	handleForgePullRequest(context.Background(), slog.Default(), stampOps{deadline: &seen}, ownerEnter(held, ring), keylock.New(), prBodyDeadline(t, reqDeadline))
	if seen.IsZero() {
		t.Fatal("no deadline on the forge call")
	}
	want := reqDeadline.Add(-forgePullRequestReserve)
	if d := seen.Sub(want); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("forge deadline %v, want about %v", seen, want)
	}
}

// TestHandleForgePullRequest_CapsFarDeadline checks that a far requester
// deadline gets the upper limit only.
func TestHandleForgePullRequest_CapsFarDeadline(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	var seen time.Time
	before := time.Now()
	handleForgePullRequest(context.Background(), slog.Default(), stampOps{deadline: &seen}, ownerEnter(held, ring), keylock.New(), prBodyDeadline(t, before.Add(time.Hour)))
	if seen.IsZero() {
		t.Fatal("no deadline on the forge call")
	}
	if d := seen.Sub(before); d < forgePullRequestTimeout || d > forgePullRequestTimeout+2*time.Second {
		t.Fatalf("deadline %v after the call start, want about %v", d, forgePullRequestTimeout)
	}
}

// TestHandleForgePullRequest_ExpiredRefusesWithoutCall checks that a request
// with too little time left is refused before any forge call.
func TestHandleForgePullRequest_ExpiredRefusesWithoutCall(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	var calls atomic.Int32
	got := handleForgePullRequest(context.Background(), slog.Default(), stampOps{calls: &calls}, ownerEnter(held, ring), keylock.New(),
		prBodyDeadline(t, time.Now().Add(forgePullRequestReserve+5*time.Second)))
	var rep forgeapi.PullRequestReply
	if err := json.Unmarshal(got, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Code != forgeapi.CodeRequestExpired {
		t.Fatalf("code %q, want %q", rep.Code, forgeapi.CodeRequestExpired)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("forge calls %d, want 0", n)
	}
}

// TestHandleForgePullRequest_MissingDeadlineIsInvalid checks that a request
// without a deadline is refused before any forge call.
func TestHandleForgePullRequest_MissingDeadlineIsInvalid(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	var calls atomic.Int32
	got := handleForgePullRequest(context.Background(), slog.Default(), stampOps{calls: &calls}, ownerEnter(held, ring), keylock.New(), prBodyDeadline(t, time.Time{}))
	var rep forgeapi.PullRequestReply
	if err := json.Unmarshal(got, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Code != forgeapi.CodeInvalidRequest {
		t.Fatalf("code %q, want %q", rep.Code, forgeapi.CodeInvalidRequest)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("forge calls %d, want 0", n)
	}
}

func issueBody(t *testing.T) []byte {
	t.Helper()
	return issueBodyDeadline(t, time.Now().Add(forgeapi.RequestTimeout))
}

// issueBodyDeadline returns an issue request that carries the deadline d.
func issueBodyDeadline(t *testing.T, d time.Time) []byte {
	t.Helper()
	b, err := json.Marshal(forgeapi.IssueRequest{Team: "payments", Title: "t", Body: "b", Deadline: d})
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
func startForgeReplica(t *testing.T, url, id, verb string, ring *sharding.Ring, held map[int]bool, ops stampOps) {
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
	locks := keylock.New()
	r := &forgeRunner{
		busConn: conn,
		cluster: "core",
		verb:    verb,
		limit:   forgeConcurrency,
		logger:  logger,
		handle: func(ctx context.Context, data []byte) []byte {
			switch verb {
			case forgeapi.VerbCheck:
				return handleForgeCheck(ctx, logger, ops, enter, data)
			case forgeapi.VerbPullRequest:
				return handleForgePullRequest(ctx, logger, ops, enter, locks, data)
			case forgeapi.VerbIssue:
				return handleForgeIssue(ctx, logger, ops, enter, "varroa-system", data)
			}
			return handleForgeRead(ctx, logger, ops, enter, data)
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
	for _, verb := range []string{forgeapi.VerbCheck, forgeapi.VerbRead, forgeapi.VerbPullRequest} {
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
			startForgeReplica(t, url, "A", verb, ring, aHeld, stampOps{id: "A"})
			startForgeReplica(t, url, "B", verb, ring, bHeld, stampOps{id: "B"})

			client, err := nats.Connect(url)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.Close)
			subject := bus.OperatorForgeSubject("core", verb)
			body := checkBody(t)
			switch verb {
			case forgeapi.VerbRead:
				body = readBody(t)
			case forgeapi.VerbPullRequest:
				body = prBody(t)
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

// TestForgeRunners_QueuedPullRequestExpires checks that a request which waits
// for its Team behind a slow pull request is refused once its deadline leaves
// too little work time, and that the forge makes no call for it.
func TestForgeRunners_QueuedPullRequestExpires(t *testing.T) {
	s := startLedgerActionNATS(t)
	url := s.ClientURL()
	ring := sharding.NewRing(8)
	own := ring.ShardFor(forgeops.ShardKey("payments"))
	held := map[int]bool{own: true}
	var calls atomic.Int32
	block := make(chan struct{})
	startForgeReplica(t, url, "A", forgeapi.VerbPullRequest, ring, held, stampOps{id: "A", calls: &calls, block: block})

	client, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	subject := bus.OperatorForgeSubject("core", forgeapi.VerbPullRequest)

	// The first request holds the Team lock until the test closes block.
	inboxA := client.NewRespInbox()
	subA, err := client.SubscribeSync(inboxA)
	if err != nil {
		t.Fatal(err)
	}
	// The runner subscribes in its own goroutine, so an early publish is
	// dropped. Publish again until the forge sees the request.
	bodyA := prBodyDeadline(t, time.Now().Add(forgeapi.PullRequestTimeout))
	for i := 0; i < 50 && calls.Load() == 0; i++ {
		if err := client.PublishRequest(subject, inboxA, bodyA); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatal("the first request did not reach the forge")
	}

	// The second request has reserve + min budget + 1 s left when it is sent.
	// The first request holds the Team lock for 2 s, so the wait for the
	// Team ends with less than the minimum budget left.
	inboxB := client.NewRespInbox()
	subB, err := client.SubscribeSync(inboxB)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PublishRequest(subject, inboxB, prBodyDeadline(t, time.Now().Add(forgePullRequestReserve+forgePullRequestMinBudget+time.Second))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	close(block)

	msgA, err := nextQueuedReply(subA, 5*time.Second)
	if err != nil {
		t.Fatalf("no reply to the first request: %v", err)
	}
	var repA forgeapi.PullRequestReply
	if err := json.Unmarshal(msgA.Data, &repA); err != nil {
		t.Fatal(err)
	}
	if !repA.OK {
		t.Fatalf("first request failed: %+v", repA.Status)
	}
	msgB, err := nextQueuedReply(subB, 5*time.Second)
	if err != nil {
		t.Fatalf("no reply to the queued request: %v", err)
	}
	var repB forgeapi.PullRequestReply
	if err := json.Unmarshal(msgB.Data, &repB); err != nil {
		t.Fatal(err)
	}
	if repB.Code != forgeapi.CodeRequestExpired {
		t.Fatalf("queued request code %q, want %q", repB.Code, forgeapi.CodeRequestExpired)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("forge calls %d, want 1", n)
	}
}

// nextQueuedReply returns the next reply on sub. It skips the no-responders
// status that the server sends for a publish that arrived before the runner
// subscribed.
func nextQueuedReply(sub *nats.Subscription, wait time.Duration) (*nats.Msg, error) {
	deadline := time.Now().Add(wait)
	for {
		msg, err := sub.NextMsg(time.Until(deadline))
		if err == nats.ErrNoResponders {
			continue
		}
		return msg, err
	}
}

// TestHandleForgeIssue_DeadlineLeavesReserve checks that the forge call ends
// the issue reserve before the requester's deadline.
func TestHandleForgeIssue_DeadlineLeavesReserve(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	var seen time.Time
	reqDeadline := time.Now().Add(30 * time.Second)
	handleForgeIssue(context.Background(), slog.Default(), stampOps{deadline: &seen}, ownerEnter(held, ring), "varroa-system", issueBodyDeadline(t, reqDeadline))
	if seen.IsZero() {
		t.Fatal("no deadline on the forge call")
	}
	want := reqDeadline.Add(-forgeIssueReserve)
	if d := seen.Sub(want); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("forge deadline %v, want about %v", seen, want)
	}
}

// TestHandleForgeIssue_CapsFarDeadline checks that a far requester deadline
// gets the issue upper limit only.
func TestHandleForgeIssue_CapsFarDeadline(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	var seen time.Time
	before := time.Now()
	handleForgeIssue(context.Background(), slog.Default(), stampOps{deadline: &seen}, ownerEnter(held, ring), "varroa-system", issueBodyDeadline(t, before.Add(time.Hour)))
	if seen.IsZero() {
		t.Fatal("no deadline on the forge call")
	}
	if d := seen.Sub(before); d < forgeRequestTimeout || d > forgeRequestTimeout+2*time.Second {
		t.Fatalf("deadline %v after the call start, want about %v", d, forgeRequestTimeout)
	}
}

// TestHandleForgeIssue_ExpiredRefusesWithoutCall checks that an issue with too
// little time left is refused before any forge call.
func TestHandleForgeIssue_ExpiredRefusesWithoutCall(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	var calls atomic.Int32
	got := handleForgeIssue(context.Background(), slog.Default(), stampOps{calls: &calls}, ownerEnter(held, ring), "varroa-system",
		issueBodyDeadline(t, time.Now().Add(forgeIssueReserve+2*time.Second)))
	var rep forgeapi.IssueReply
	if err := json.Unmarshal(got, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Code != forgeapi.CodeRequestExpired {
		t.Fatalf("code %q, want %q", rep.Code, forgeapi.CodeRequestExpired)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("forge calls %d, want 0", n)
	}
}

// TestHandleForgeIssue_MissingDeadlineIsInvalid checks that an issue without a
// deadline is refused before any forge call.
func TestHandleForgeIssue_MissingDeadlineIsInvalid(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	var calls atomic.Int32
	got := handleForgeIssue(context.Background(), slog.Default(), stampOps{calls: &calls}, ownerEnter(held, ring), "varroa-system", issueBodyDeadline(t, time.Time{}))
	var rep forgeapi.IssueReply
	if err := json.Unmarshal(got, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Code != forgeapi.CodeInvalidRequest {
		t.Fatalf("code %q, want %q", rep.Code, forgeapi.CodeInvalidRequest)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("forge calls %d, want 0", n)
	}
}

// TestForgeRunners_QueuedIssueExpires checks that an issue which waits in the
// queue behind four slow ones is refused once its deadline leaves too little
// work time, and that the forge makes no call for it.
func TestForgeRunners_QueuedIssueExpires(t *testing.T) {
	s := startLedgerActionNATS(t)
	url := s.ClientURL()
	ring := sharding.NewRing(8)
	own := ring.ShardFor(forgeops.ShardKey("payments"))
	held := map[int]bool{own: true}
	var calls atomic.Int32
	block := make(chan struct{})
	startForgeReplica(t, url, "A", forgeapi.VerbIssue, ring, held, stampOps{id: "A", calls: &calls, block: block})

	client, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	subject := bus.OperatorForgeSubject("core", forgeapi.VerbIssue)

	// The first four requests hold the four slots until the test closes block.
	// The runner subscribes in its own goroutine, so an early publish is
	// dropped. Publish again until the forge sees the request.
	inboxFirst := client.NewRespInbox()
	if _, err := client.SubscribeSync(inboxFirst); err != nil {
		t.Fatal(err)
	}
	bodyFirst := issueBodyDeadline(t, time.Now().Add(forgeapi.RequestTimeout))
	for i := 0; i < 50 && calls.Load() == 0; i++ {
		if err := client.PublishRequest(subject, inboxFirst, bodyFirst); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatal("the first request did not reach the forge")
	}
	for i := 0; i < 3; i++ {
		inbox := client.NewRespInbox()
		if _, err := client.SubscribeSync(inbox); err != nil {
			t.Fatal(err)
		}
		if err := client.PublishRequest(subject, inbox, issueBodyDeadline(t, time.Now().Add(forgeapi.RequestTimeout))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50 && calls.Load() < 4; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if calls.Load() != 4 {
		t.Fatalf("forge calls %d, want 4 before the queued request", calls.Load())
	}

	// The queued request has reserve + min budget + 1 s left when it is sent.
	// The four slots hold for 2 s, so less than the minimum budget is left
	// when the queued request gets a slot.
	inboxB := client.NewRespInbox()
	subB, err := client.SubscribeSync(inboxB)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PublishRequest(subject, inboxB, issueBodyDeadline(t, time.Now().Add(forgeIssueReserve+forgeIssueMinBudget+time.Second))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	close(block)

	msgB, err := nextQueuedReply(subB, 5*time.Second)
	if err != nil {
		t.Fatalf("no reply to the queued request: %v", err)
	}
	var repB forgeapi.IssueReply
	if err := json.Unmarshal(msgB.Data, &repB); err != nil {
		t.Fatal(err)
	}
	if repB.Code != forgeapi.CodeRequestExpired {
		t.Fatalf("queued request code %q, want %q", repB.Code, forgeapi.CodeRequestExpired)
	}
	if n := calls.Load(); n != 4 {
		t.Fatalf("forge calls %d, want 4", n)
	}
}

// stampOf returns the replica id that a reply carries.
func stampOf(t *testing.T, verb string, data []byte) string {
	t.Helper()
	if verb == forgeapi.VerbPullRequest {
		var rep forgeapi.PullRequestReply
		if err := json.Unmarshal(data, &rep); err != nil {
			t.Fatal(err)
		}
		return rep.URL
	}
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

// TestForgeRunners_OtherTeamPullRequestRunsBesideASlowOne checks that a slow
// pull request for one Team does not hold back a pull request for another Team
// on the same replica.
func TestForgeRunners_OtherTeamPullRequestRunsBesideASlowOne(t *testing.T) {
	s := startLedgerActionNATS(t)
	url := s.ClientURL()
	ring := sharding.NewRing(8)
	held := map[int]bool{
		ring.ShardFor(forgeops.ShardKey("payments")): true,
		ring.ShardFor(forgeops.ShardKey("billing")):  true,
	}
	var calls atomic.Int32
	block := make(chan struct{})
	startForgeReplica(t, url, "A", forgeapi.VerbPullRequest, ring, held,
		stampOps{id: "A", calls: &calls, block: block, blockTeam: "payments"})

	client, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	subject := bus.OperatorForgeSubject("core", forgeapi.VerbPullRequest)

	inboxA := client.NewRespInbox()
	subA, err := client.SubscribeSync(inboxA)
	if err != nil {
		t.Fatal(err)
	}
	// The runner subscribes in its own goroutine, so an early publish is
	// dropped. Publish again until the forge sees the request.
	bodyA := prBodyTeam(t, "payments", time.Now().Add(forgeapi.PullRequestTimeout))
	for i := 0; i < 50 && calls.Load() == 0; i++ {
		if err := client.PublishRequest(subject, inboxA, bodyA); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatal("the payments request did not reach the forge")
	}

	inboxB := client.NewRespInbox()
	subB, err := client.SubscribeSync(inboxB)
	if err != nil {
		t.Fatal(err)
	}
	bodyB := prBodyTeam(t, "billing", time.Now().Add(forgeapi.PullRequestTimeout))
	if err := client.PublishRequest(subject, inboxB, bodyB); err != nil {
		t.Fatal(err)
	}
	msgB, err := nextQueuedReply(subB, 3*time.Second)
	if err != nil {
		t.Fatalf("the billing request waited behind the payments request: %v", err)
	}
	var repB forgeapi.PullRequestReply
	if err := json.Unmarshal(msgB.Data, &repB); err != nil {
		t.Fatal(err)
	}
	if !repB.OK {
		t.Fatalf("billing request failed: %+v", repB.Status)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("forge calls %d while payments is held, want 2", n)
	}

	close(block)
	msgA, err := nextQueuedReply(subA, 5*time.Second)
	if err != nil {
		t.Fatalf("no reply to the payments request: %v", err)
	}
	var repA forgeapi.PullRequestReply
	if err := json.Unmarshal(msgA.Data, &repA); err != nil {
		t.Fatal(err)
	}
	if !repA.OK {
		t.Fatalf("payments request failed: %+v", repA.Status)
	}
}

// TestHandleForgePullRequest_WaitsForTeamThenRuns checks that a pull request
// waits for the lock of its Team, and runs once the lock is free and enough
// time is left.
func TestHandleForgePullRequest_WaitsForTeamThenRuns(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	locks := keylock.New()
	key := forgeops.ShardKey("payments")
	if err := locks.Lock(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	body := prBodyDeadline(t, time.Now().Add(forgePullRequestReserve+forgePullRequestMinBudget+3*time.Second))
	done := make(chan []byte, 1)
	go func() {
		done <- handleForgePullRequest(context.Background(), slog.Default(), stampOps{calls: &calls}, ownerEnter(held, ring), locks, body)
	}()
	time.Sleep(500 * time.Millisecond)
	locks.Unlock(key)

	var rep forgeapi.PullRequestReply
	if err := json.Unmarshal(<-done, &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("reply %+v, want OK after the Team lock was freed", rep.Status)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("forge calls %d, want 1", n)
	}
}

// TestHandleForgePullRequest_TeamWaitExpiresWithoutCall checks that a pull
// request whose wait for its Team leaves less than the minimum budget answers
// request_expired and makes no forge call.
func TestHandleForgePullRequest_TeamWaitExpiresWithoutCall(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	locks := keylock.New()
	key := forgeops.ShardKey("payments")
	if err := locks.Lock(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	defer locks.Unlock(key)
	var calls atomic.Int32
	body := prBodyDeadline(t, time.Now().Add(forgePullRequestReserve+forgePullRequestMinBudget+time.Second))
	start := time.Now()
	got := handleForgePullRequest(context.Background(), slog.Default(), stampOps{calls: &calls}, ownerEnter(held, ring), locks, body)
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("the wait took %v, want about 1s", waited)
	}
	var rep forgeapi.PullRequestReply
	if err := json.Unmarshal(got, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Code != forgeapi.CodeRequestExpired {
		t.Fatalf("code %q, want %q", rep.Code, forgeapi.CodeRequestExpired)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("forge calls %d, want 0", n)
	}
	if n := locks.Size(); n != 1 {
		t.Fatalf("lock entries %d, want 1 for the holder", n)
	}
}

// TestHandleForgePullRequest_OtherTeamIgnoresHeldLock checks that a lock held
// for one Team does not delay a pull request for another Team.
func TestHandleForgePullRequest_OtherTeamIgnoresHeldLock(t *testing.T) {
	ring := sharding.NewRing(8)
	held := map[int]bool{ring.ShardFor(forgeops.ShardKey("payments")): true}
	locks := keylock.New()
	if err := locks.Lock(context.Background(), forgeops.ShardKey("billing")); err != nil {
		t.Fatal(err)
	}
	defer locks.Unlock(forgeops.ShardKey("billing"))
	var calls atomic.Int32
	start := time.Now()
	got := handleForgePullRequest(context.Background(), slog.Default(), stampOps{calls: &calls}, ownerEnter(held, ring), locks, prBody(t))
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("the call took %v behind another Team's lock", waited)
	}
	var rep forgeapi.PullRequestReply
	if err := json.Unmarshal(got, &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("reply %+v, want OK", rep.Status)
	}
}
