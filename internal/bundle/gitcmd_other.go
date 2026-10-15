//go:build !unix

package bundle

import (
	"context"
	"os/exec"
	"time"
)

// gitGroupKill reports that a cancel does not kill the transport helper children.
const gitGroupKill = false

// gitCommand returns a git command that stops when ctx is done. This
// fallback has no process-group kill: a cancel kills only the git process,
// and a transport helper child can continue to run. The wait delay makes
// Wait return when such a child holds an output pipe open.
func gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}
