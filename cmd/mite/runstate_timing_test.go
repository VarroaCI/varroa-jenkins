package main

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// asyncFetchStream answers every run_state_request the way the real
// transport does: Send returns at once, and a separate goroutine
// delivers the response, as the mite's own stream reader does. The
// existing helper answers inside Send, which hides every ordering
// defect this shape can have.
type asyncFetchStream struct {
	concurrentSendStream
	a    *Agent
	data []byte
	t    *testing.T
}

func newAsyncFetchStream(t *testing.T, a *Agent, data []byte) *asyncFetchStream {
	s := &asyncFetchStream{a: a, data: data, t: t}
	s.sendFn = func(m *mitev1.MiteMessage) error {
		req := m.GetRunStateRequest()
		if req == nil {
			return nil
		}
		gz := gzipBytes(t, data)
		go func() {
			s.a.runStateWaiters.resolve(&mitev1.RunStateResponse{
				RequestId: req.RequestId, Relpath: req.Relpath,
				Seq: 0, Total: 1, GzipContent: gz, RawSize: int64(len(data)),
			})
		}()
		return nil
	}
	return s
}

// TestResumeFetchesManyFilesQuickly measures the fetch half of a resume
// over a realistic set. The live run took six and a half minutes for
// about twenty five files, so this bounds the same work well under a
// minute.
func TestResumeFetchesManyFilesQuickly(t *testing.T) {
	agent := mustNewAgent(t, Config{JenkinsURL: "http://localhost:8080", CellInstance: "pod-live"})
	agent.Logger = slog.Default()
	agent.jenkinsHomeDir = t.TempDir()
	setTestStream(agent, newAsyncFetchStream(t, agent, []byte("<flow/>")))

	start := time.Now()
	for i := 0; i < 25; i++ {
		relPath := fmt.Sprintf("jobs/team/jobs/pipeline/builds/7/workflow/%d.xml", i+1)
		if _, err := agent.fetchRunStateFileWithRetry(context.Background(), "run-1", relPath, testPusherEpoch, testPusherGen); err != nil {
			t.Fatalf("fetch %s: %v", relPath, err)
		}
	}
	elapsed := time.Since(start)
	t.Logf("fetched 25 files in %s", elapsed)
	if elapsed > 30*time.Second {
		t.Fatalf("25 fetches took %s, want well under a minute", elapsed)
	}
}

// TestFetchRunStateFileRejectsAnUnnamedPush proves the mite fails a
// fetch whose file names no pusher epoch or no push generation. The
// gateway refuses such a request every time, so a retry would spend the
// command's deadline on an answer that never changes.
func TestFetchRunStateFileRejectsAnUnnamedPush(t *testing.T) {
	agent := mustNewAgent(t, Config{JenkinsURL: "http://localhost:8080", CellInstance: "pod-live"})
	agent.Logger = slog.Default()

	for _, tc := range []struct {
		name       string
		epoch, gen int64
	}{
		{"no epoch", 0, 4},
		{"no generation", 100, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := agent.fetchRunStateFileWithRetry(
				context.Background(), "run-1", "jobs/team/builds/7/build.xml", tc.epoch, tc.gen)
			if err == nil {
				t.Fatal("expected a fetch that names no push to fail at once")
			}
		})
	}
}
