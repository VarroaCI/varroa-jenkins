package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/logbuffer"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// podNameRecordingClient embeds the ResourceClient interface (so it satisfies
// the full method set) but overrides only the two methods the log handlers use,
// recording the pod name and read options passed to StreamPodLogs.
type podNameRecordingClient struct {
	controller.ResourceClient
	cr          *v1alpha1.Controller
	getErr      error
	gotPodNames []string
	gotOpts     []corev1.PodLogOptions
}

func (c *podNameRecordingClient) GetControllerCRD(_ context.Context, _, _ string) (*v1alpha1.Controller, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	return c.cr, nil
}

func (c *podNameRecordingClient) StreamPodLogs(_ context.Context, _, podName string, opts corev1.PodLogOptions) (io.ReadCloser, error) {
	c.gotPodNames = append(c.gotPodNames, podName)
	c.gotOpts = append(c.gotOpts, opts)
	return io.NopCloser(strings.NewReader("2026-11-04T10:00:00.5Z line\n")), nil
}

// fetchPodLogsOneShot must address the UID-named StatefulSet pod
// ("<name>-<uid8>-0"), not the bare "<name>-0" — see controller.PodName.
func TestFetchPodLogsOneShot_UsesUIDPodName(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "smoke-qwen3",
			Namespace: "varroa",
			UID:       "54c62aff-0000-0000-0000-000000000000",
		},
	}
	rc := &podNameRecordingClient{cr: cr}
	st := crdstore.NewFake()
	crdstore.MustSeed(st, cr)
	s := NewServer(&Dependencies{
		Client: rc,
		Store:  st,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	s.fetchPodLogsOneShot(context.Background(), "varroa", "smoke-qwen3")

	want := controller.PodName(cr, 0) // smoke-qwen3-54c62aff-0
	if len(rc.gotPodNames) == 0 {
		t.Fatal("StreamPodLogs was never called")
	}
	for _, got := range rc.gotPodNames {
		if got != want {
			t.Errorf("StreamPodLogs pod name = %q, want %q", got, want)
		}
		if got == cr.Name+"-0" {
			t.Errorf("StreamPodLogs used bare-name pod %q (the regression)", got)
		}
	}
}

// fetchPodLogsOneShot reads each line with its kubelet timestamp, so the
// entry carries the time the container runtime wrote the line.
func TestFetchPodLogsOneShot_UsesKubeletTimestamps(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "smoke", Namespace: "varroa", UID: "54c62aff-0000-0000-0000-000000000000"},
	}
	rc := &podNameRecordingClient{cr: cr}
	st := crdstore.NewFake()
	crdstore.MustSeed(st, cr)
	s := NewServer(&Dependencies{
		Client: rc,
		Store:  st,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	entries := s.fetchPodLogsOneShot(context.Background(), "varroa", "smoke")

	if len(entries) == 0 {
		t.Fatal("fetchPodLogsOneShot returned no entries")
	}
	wantTS, err := time.Parse(time.RFC3339Nano, "2026-11-04T10:00:00.5Z")
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Message != "line" {
		t.Errorf("Message = %q, want %q", entries[0].Message, "line")
	}
	if !entries[0].Timestamp.Equal(wantTS) {
		t.Errorf("Timestamp = %v, want %v", entries[0].Timestamp, wantTS)
	}
	opts := rc.gotOpts[0]
	if !opts.Timestamps {
		t.Error("one-shot read must set Timestamps")
	}
	if opts.Follow {
		t.Error("one-shot read must not follow")
	}
	if opts.TailLines == nil || *opts.TailLines != 500 {
		t.Errorf("TailLines = %v, want 500", opts.TailLines)
	}
}

// On pod-name resolution failure fetchPodLogsOneShot must still return a
// non-nil empty slice — callers JSON-encode it directly and the success path
// never returns nil, so nil would serialize as "null" and break the shape.
func TestFetchPodLogsOneShot_ResolveError_ReturnsEmptySlice(t *testing.T) {
	rc := &podNameRecordingClient{getErr: errors.New("not found")}
	s := NewServer(&Dependencies{
		Client: rc,
		Store:  crdstore.NewFake(), // empty: controller lookup fails with NotFound
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	entries := s.fetchPodLogsOneShot(context.Background(), "varroa", "missing")

	if entries == nil {
		t.Fatal("fetchPodLogsOneShot returned nil; want non-nil empty slice")
	}
	if len(entries) != 0 {
		t.Errorf("len(entries) = %d, want 0", len(entries))
	}
	if len(rc.gotPodNames) != 0 {
		t.Errorf("StreamPodLogs should not be called when resolution fails, got %v", rc.gotPodNames)
	}
}

// scriptStep is the outcome of one StreamPodLogs call: a body or an open error.
type scriptStep struct {
	body string
	err  error
}

// scriptedLogClient answers StreamPodLogs from a script. Calls past the script
// return a stream that stays open until the context ends. It records the read
// options of every call.
type scriptedLogClient struct {
	controller.ResourceClient
	steps []scriptStep

	mu    sync.Mutex
	calls []corev1.PodLogOptions
}

func (c *scriptedLogClient) StreamPodLogs(ctx context.Context, _, _ string, opts corev1.PodLogOptions) (io.ReadCloser, error) {
	c.mu.Lock()
	i := len(c.calls)
	c.calls = append(c.calls, opts)
	c.mu.Unlock()

	if i >= len(c.steps) {
		pr, pw := io.Pipe()
		go func() {
			<-ctx.Done()
			_ = pw.Close()
		}()
		return pr, nil
	}
	step := c.steps[i]
	if step.err != nil {
		return nil, step.err
	}
	return io.NopCloser(strings.NewReader(step.body)), nil
}

func (c *scriptedLogClient) readOpts() []corev1.PodLogOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]corev1.PodLogOptions(nil), c.calls...)
}

// runFollow starts followContainerLogs and returns the lines it sends. It
// collects want lines, then cancels the stream and waits for it to end.
func runFollow(t *testing.T, client *scriptedLogClient, want int) []logbuffer.LogEntry {
	t.Helper()
	s := NewServer(&Dependencies{
		Client: client,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	s.podLogRetry = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan logbuffer.LogEntry, 100)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.followContainerLogs(ctx, "varroa", "smoke-abc-0", "jenkins", out)
	}()

	var got []logbuffer.LogEntry
	timeout := time.After(5 * time.Second)
	for len(got) < want {
		select {
		case e := <-out:
			got = append(got, e)
		case <-timeout:
			cancel()
			<-done
			t.Fatalf("timed out after %d of %d lines", len(got), want)
		}
	}
	cancel()
	<-done
	return got
}

func mustKubeletTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// A resumed read sends each line once. The API repeats the lines of the
// cursor second, so the stream drops them.
func TestFollowContainerLogs_ResumeSendsEachLineOnce(t *testing.T) {
	client := &scriptedLogClient{steps: []scriptStep{
		{body: "2026-11-04T10:00:00.1Z one\n2026-11-04T10:00:01.2Z two\n"},
		{body: "2026-11-04T10:00:01.2Z two\n2026-11-04T10:00:02Z three\n2026-11-04T10:00:02Z four\n"},
		{body: "2026-11-04T10:00:02Z three\n2026-11-04T10:00:02Z four\n2026-11-04T10:00:02Z four-b\n2026-11-04T10:00:03Z five\n"},
	}}

	got := runFollow(t, client, 6)

	wantMsgs := []string{"one", "two", "three", "four", "four-b", "five"}
	msgs := make([]string, 0, len(got))
	for _, e := range got {
		msgs = append(msgs, e.Message)
		if e.Source != "jenkins" {
			t.Errorf("Source = %q, want jenkins", e.Source)
		}
	}
	if strings.Join(msgs, " ") != strings.Join(wantMsgs, " ") {
		t.Fatalf("messages = %v, want %v", msgs, wantMsgs)
	}
	if !got[0].Timestamp.Equal(mustKubeletTime(t, "2026-11-04T10:00:00.1Z")) {
		t.Errorf("first timestamp = %v, want the kubelet time", got[0].Timestamp)
	}

	calls := client.readOpts()
	if len(calls) < 3 {
		t.Fatalf("got %d reads, want at least 3", len(calls))
	}
	first := calls[0]
	if first.TailLines == nil || *first.TailLines != 50 {
		t.Errorf("first read TailLines = %v, want 50", first.TailLines)
	}
	if first.SinceTime != nil {
		t.Errorf("first read SinceTime = %v, want nil", first.SinceTime)
	}
	if !first.Follow || !first.Timestamps {
		t.Errorf("first read Follow=%v Timestamps=%v, want both true", first.Follow, first.Timestamps)
	}
	second := calls[1]
	if second.TailLines != nil {
		t.Errorf("second read TailLines = %v, want nil", *second.TailLines)
	}
	if second.SinceTime == nil || !second.SinceTime.Time.Equal(mustKubeletTime(t, "2026-11-04T10:00:01.2Z")) {
		t.Errorf("second read SinceTime = %v, want 10:00:01.2", second.SinceTime)
	}
	third := calls[2]
	if third.SinceTime == nil || !third.SinceTime.Time.Equal(mustKubeletTime(t, "2026-11-04T10:00:02Z")) {
		t.Errorf("third read SinceTime = %v, want 10:00:02", third.SinceTime)
	}
}

// A read that fails to open before any line tails again. No line was sent, so
// there is no cursor to resume from.
func TestFollowContainerLogs_FailedOpenBeforeAnyLineTailsAgain(t *testing.T) {
	client := &scriptedLogClient{steps: []scriptStep{
		{err: errors.New("container not ready")},
		{body: "2026-11-04T10:00:00Z one\n"},
	}}

	got := runFollow(t, client, 1)
	if got[0].Message != "one" {
		t.Errorf("Message = %q, want one", got[0].Message)
	}

	calls := client.readOpts()
	if len(calls) < 2 {
		t.Fatalf("got %d reads, want at least 2", len(calls))
	}
	if calls[1].TailLines == nil || *calls[1].TailLines != 50 {
		t.Errorf("read after failed open TailLines = %v, want 50", calls[1].TailLines)
	}
	if calls[1].SinceTime != nil {
		t.Errorf("read after failed open SinceTime = %v, want nil", calls[1].SinceTime)
	}
}

func TestSplitKubeletTimestamp(t *testing.T) {
	ts := mustKubeletTime(t, "2026-11-04T10:00:00.5Z")
	tsWhole := mustKubeletTime(t, "2026-11-04T10:00:00Z")
	tests := []struct {
		name    string
		line    string
		wantTS  time.Time
		wantMsg string
		wantOK  bool
	}{
		{"nanosecond prefix", "2026-11-04T10:00:00.5Z hello", ts, "hello", true},
		{"prefix without fraction", "2026-11-04T10:00:00Z hello", tsWhole, "hello", true},
		{"empty message", "2026-11-04T10:00:00Z ", tsWhole, "", true},
		{"timestamp with no space", "2026-11-04T10:00:00Z", tsWhole, "", true},
		{"non-time prefix", "hello world", time.Time{}, "hello world", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTS, gotMsg, gotOK := splitKubeletTimestamp(tt.line)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotMsg != tt.wantMsg {
				t.Errorf("msg = %q, want %q", gotMsg, tt.wantMsg)
			}
			if tt.wantOK && !gotTS.Equal(tt.wantTS) {
				t.Errorf("ts = %v, want %v", gotTS, tt.wantTS)
			}
		})
	}
}
