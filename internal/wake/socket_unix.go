//go:build !windows

package wake

import (
	"net"
	"time"
)

// dial connects to a Unix domain socket with a short timeout. A wake signal
// must never stall a harness hook.
func dial(path string) (net.Conn, error) {
	return net.DialTimeout("unix", path, 2*time.Second)
}
