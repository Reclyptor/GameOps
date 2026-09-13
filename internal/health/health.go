// Package health is the one health check, shared by `gameops health` (the
// image HEALTHCHECK / exec probe) and the /healthz endpoint.
package health

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/Reclyptor/GameOps/internal/adapter"
	"github.com/Reclyptor/GameOps/internal/state"
)

// Check returns nil when the server is healthy. An adapter that overrides
// game_healthy decides on its own; otherwise the server must be running,
// ready, and — for TCP games — accepting connections on GAME_PORT.
func Check(st *state.Store, ad *adapter.Adapter, vars map[string]string) error {
	if res, err := ad.Call("game_healthy"); err == nil && !res.Unsupported() {
		if res.Code != 0 {
			return fmt.Errorf("game_healthy reported unhealthy (rc=%d)", res.Code)
		}
		return nil
	}
	if st.ServerPID() == 0 {
		return fmt.Errorf("server process not running")
	}
	if !st.HasFlag("ready") {
		return fmt.Errorf("server not ready")
	}
	if port := vars["GAME_PORT"]; port != "" && strings.EqualFold(vars["GAME_PORT_PROTO"], "tcp") {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 3*time.Second)
		if err != nil {
			return fmt.Errorf("tcp/%s not accepting connections", port)
		}
		c.Close()
	}
	return nil
}
