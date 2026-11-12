//go:build !windows

package wake

import (
	"os/exec"
	"syscall"
)

// detach puts the child in its own process group so a hook's timeout cannot
// kill the turn it started, and so the turn outlives the short-lived process
// that asked for it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
