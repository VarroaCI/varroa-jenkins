package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/internal/api/logbuffer"
)

const (
	podLogFirstTail    int64 = 50  // lines the first read of a container shows
	podLogOneShotTail  int64 = 500 // lines a one-shot read of a container shows
	podLogRetryDefault       = 10 * time.Second
	// podLogLineCap is the most bytes of one log line that a frame carries.
	// json.Marshal can write six bytes for one input byte, so a capped line
	// stays under the 1 MiB SSE line limit of pkg/client/stream.
	podLogLineCap    = 128 << 10
	podLogReadBuffer = 64 << 10
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

// readPodLogLine reads the next line from br. It keeps at most podLogLineCap
// bytes of the line and reads past the rest. size is the full length of the
// line without its line end. complete is false when the read ended before a
// newline; err is then the error that ended the read (io.EOF for a clean end).
func readPodLogLine(br *bufio.Reader) (line []byte, size int, complete bool, err error) {
	for {
		frag, rerr := br.ReadSlice('\n')
		if rerr == nil {
			frag = frag[:len(frag)-1]
			if len(frag) > 0 && frag[len(frag)-1] == '\r' {
				frag = frag[:len(frag)-1]
			}
		}
		size += len(frag)
		if room := podLogLineCap - len(line); room > 0 {
			line = append(line, frag[:min(len(frag), room)]...)
		}
		switch {
		case rerr == nil:
			return line, size, true, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		default:
			return line, size, false, rerr
		}
	}
}

// podLogLineText returns the text of a line. A line longer than podLogLineCap
// ends with a note that gives its full size. The cut can split a multi-byte
// rune; the text drops that partial rune and keeps every other byte.
func podLogLineText(line []byte, size int) string {
	if size <= podLogLineCap {
		return string(line)
	}
	return string(trimPartialRune(line)) + " … [truncated, " + podLogSize(size) + " line]"
}

// trimPartialRune drops a multi-byte rune that a cut left incomplete at the end.
func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			return b
		}
	}
	return b
}

func podLogSize(n int) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", n>>10)
}

// podLogLines reads r line by line and calls emit with the text of each line,
// so a long line never stops the read. A last line without a newline goes to
// emit only when the read ends with io.EOF. After any other read error the
// part is dropped: a follow read starts again at its cursor and reads the
// whole line. podLogLines returns the read error, or nil for io.EOF and for a
// stop by emit.
func podLogLines(r io.Reader, emit func(line string) bool) error {
	br := bufio.NewReaderSize(r, podLogReadBuffer)
	for {
		line, size, complete, err := readPodLogLine(br)
		if !complete {
			if errors.Is(err, io.EOF) {
				if size > 0 {
					emit(podLogLineText(line, size))
				}
				return nil
			}
			return err
		}
		if !emit(podLogLineText(line, size)) {
			return nil
		}
	}
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
// A line that a failed read cuts off is not sent; the next read sends it whole.
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
			stopped := false
			readErr := podLogLines(rc, func(line string) bool {
				entry, stamped := podLogEntry(container, line)
				if stamped && filter.drop(entry.Timestamp) {
					return true
				}
				select {
				case out <- entry:
				case <-ctx.Done():
					stopped = true
					return false
				}
				if stamped {
					cur.sent(entry.Timestamp)
				}
				return true
			})
			_ = rc.Close()
			if stopped {
				return
			}
			if readErr != nil && ctx.Err() == nil {
				s.deps.Logger.Warn("log stream read failed", "namespace", namespace, "pod", podName,
					"container", container, "error", readErr)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.podLogRetryDelay()):
		}
	}
}
