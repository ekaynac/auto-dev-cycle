// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/ekaynac/auto-dev-cycle/internal/client"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// The environment that selects the controller transport (08 "Transport").
const (
	SocketEnv    = "ADC_SOCKET"
	DaemonUIDEnv = "ADC_DAEMON_UID"
)

// ClientFromEnv returns the Unix-socket transport to the socket ADC_SOCKET
// names, or NoTransport when it is unset (the commands' integration is PR
// 10a). The peer must run as ADC_DAEMON_UID, by default the caller's own
// uid: the private socket admits only the daemon uid [M3]. The token comes
// from ADC_TOKEN or ~/.config/adc/token [R5], read for each command.
func ClientFromEnv(getenv func(string) string) Client {
	socket := getenv(SocketEnv)
	if socket == "" {
		return NoTransport{}
	}
	uid := os.Getuid()
	if v := getenv(DaemonUIDEnv); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return failing{fmt.Errorf("%s %q is not a uid", DaemonUIDEnv, v)}
		}
		uid = int(n)
	}
	return client.Transport{Socket: socket, DaemonUID: uid, Token: func() (identity.Secret, error) { return client.Token(getenv) }}
}

// failing is the Client of a transport configured wrongly: every command
// fails with why, and commands that need no controller still run.
type failing struct{ err error }

func (f failing) Command(context.Context, string, []byte) ([]byte, error) { return nil, f.err }

func (f failing) Bootstrap(context.Context, []byte) ([]byte, error) { return nil, f.err }
