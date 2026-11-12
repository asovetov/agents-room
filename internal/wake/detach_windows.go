//go:build windows

package wake

import (
	"os/exec"
	"syscall"
)

// detach starts the child in its own process group so it is not killed with
// the hook that started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000200} // CREATE_NEW_PROCESS_GROUP
}
