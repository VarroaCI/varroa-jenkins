package api

import (
	"bufio"
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/internal/api/logbuffer"
)

const (
	podLogFirstTail    int64 = 50  // lines the first read of a container shows
	podLogOneShotTail  int64 = 500 // lines a one-shot read of a container shows
	podLogRetryDefault       = 10 * time.Second
)

func (s *Server) podLogRetryDelay() time.Duration {
	if s.podLogRetry > 0 {
		return s.podLogRetry
	}
	return podLogRetryDefault
}

// splitKubeletTimestamp splits a line read with timestamps=true: an RFC3339Nano
// time, one space, then the line. ok is false when the prefix is not a time;
// msg is then the full line.
func splitKubeletTimestamp(line string) (ts time.Time, msg string, ok bool) {
	prefix, rest, _ := strings.Cut(line, " ")
	t, err := time.Parse(time.RFC3339Nano, prefix)
	if err != nil {
		return time.Time{}, line, false
	}
	return t, rest, true
}

// podLogEntry builds the entry of one pod log line. A line without a kubelet
// timestamp carries the read time. stamped reports a kubelet timestamp.
func podLogEntry(container, line string) (entry logbuffer.LogEntry, stamped bool) {
	ts, msg, ok := splitKubeletTimestamp(line)
	if !ok {
		ts = time.Now()
	}
	return logbuffer.LogEntry{
		Timestamp: ts,
		Level:     logbuffer.DetectLogLevel(msg),
		Source:    container,
		Message:   msg,
	}, ok
}

// logCursor marks the newest line that a follow stream sent for one container.
// The API sends sinceTime in whole seconds, so a resumed read repeats lines.
type logCursor struct {
	last time.Time
	n    int // sent lines whose timestamp equals last
}

func (c *logCursor) sent(ts time.Time) {
	switch {
	case ts.After(c.last):
		c.last, c.n = ts, 1
	case ts.Equal(c.last):
		c.n++
	}
}

// readOptions selects the next read: a tail first, then a resume at last.
func (c logCursor) readOptions(container string) corev1.PodLogOptions {
	opts := corev1.PodLogOptions{Container: container, Follow: true, Timestamps: true}
	if c.last.IsZero() {
		tail := podLogFirstTail
		opts.TailLines = &tail
		return opts
	}
	opts.SinceTime = &metav1.Time{Time: c.last}
	return opts
}

// resumeFilter drops the lines of a resumed read that the stream already sent.
// A line at the same second as the cursor is sent again by the API; the filter
// drops exactly the lines of that instant that were already sent, in order.
// Known limit: when stdout and stderr write one instant at the same
// nanosecond, the order of their lines can differ between reads. The filter
// then drops or repeats one line of that instant. This is accepted.
type resumeFilter struct {
	from    logCursor
	skipped int
}

func (f *resumeFilter) drop(ts time.Time) bool {
	if f.from.last.IsZero() {
		return false
	}
	if ts.Before(f.from.last) {
		return true
	}
	if ts.Equal(f.from.last) && f.skipped < f.from.n {
		f.skipped++
		return true
	}
	return false
}

// followContainerLogs sends the lines of one container to out until ctx ends.
// A later read in the same stream resumes at the newest sent line.
func (s *Server) followContainerLogs(ctx context.Context, namespace, podName, container string, out chan<- logbuffer.LogEntry) {
	var cur logCursor
	for ctx.Err() == nil {
		filter := resumeFilter{from: cur}
		rc, err := s.deps.Client.StreamPodLogs(ctx, namespace, podName, cur.readOptions(container))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.deps.Logger.Warn("log stream failed, retrying", "namespace", namespace, "pod", podName,
				"container", container, "retry", s.podLogRetryDelay(), "error", err)
		} else {
			scanner := bufio.NewScanner(rc)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for scanner.Scan() {
				entry, stamped := podLogEntry(container, scanner.Text())
				if stamped && filter.drop(entry.Timestamp) {
					continue
				}
				select {
				case out <- entry:
				case <-ctx.Done():
					_ = rc.Close()
					return
				}
				if stamped {
					cur.sent(entry.Timestamp)
				}
			}
			if err := scanner.Err(); err != nil && ctx.Err() == nil {
				s.deps.Logger.Warn("log stream read failed", "namespace", namespace, "pod", podName,
					"container", container, "error", err)
			}
			_ = rc.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.podLogRetryDelay()):
		}
	}
}
