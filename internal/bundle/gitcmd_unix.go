//go:build unix

package bundle

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// gitWaitDelay limits how long Wait waits for the output pipes after the
// process group is killed. A child that leaves the group can hold a pipe
// open, and Wait must still return.
const gitWaitDelay = 5 * time.Second

// gitGroupKill reports that a cancel kills the transport helper children too.
const gitGroupKill = true

// gitCommand returns a git command that stops when ctx is done. git starts
// transport helpers (git-remote-https, ssh) as child processes, and a kill
// of the git process alone leaves them running with the stderr pipe open.
// The command therefore runs in its own process group, and a cancel kills
// the whole group.
func gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = gitWaitDelay
	return cmd
}
