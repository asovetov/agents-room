//go:build windows

package wake

import (
	"github.com/Microsoft/go-winio"
	"net"
	"time"
)

// dial connects to a Windows named pipe. Claude Code binds a named pipe
// instead of a Unix socket on native Windows, and go-winio speaks that.
func dial(path string) (net.Conn, error) {
	return winio.DialPipe(path, ptr(time.Second*2))
}

func ptr[T any](v T) *T { return &v }
