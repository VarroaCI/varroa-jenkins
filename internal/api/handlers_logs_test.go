package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// podNameRecordingClient embeds the ResourceClient interface (so it satisfies
// the full method set) but overrides only the two methods the log handlers use,
// recording the pod name passed to StreamPodLogs.
type podNameRecordingClient struct {
	controller.ResourceClient
	cr          *v1alpha1.Controller
	getErr      error
	gotPodNames []string
}

func (c *podNameRecordingClient) GetControllerCRD(_ context.Context, _, _ string) (*v1alpha1.Controller, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	return c.cr, nil
}

func (c *podNameRecordingClient) StreamPodLogs(_ context.Context, _, podName, _ string, _ int64, _ bool) (io.ReadCloser, error) {
	c.gotPodNames = append(c.gotPodNames, podName)
	return io.NopCloser(strings.NewReader("line\n")), nil
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
