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
	HTTPSAddrEnv = "ADC_HTTPS_ADDR" // HOST:PORT of the controller's HTTPS listener
	HTTPSCAEnv   = "ADC_HTTPS_CA"   // a PEM file of the CA certificates to trust for it
	HTTPSPinEnv  = "ADC_HTTPS_PIN"  // or sha256:<hex>, the fingerprint of its exact certificate
)

// ClientFromEnv returns the transport every command is sent through: the
// Unix socket ADC_SOCKET names, the HTTPS listener at ADC_HTTPS_ADDR, or
// NoTransport when neither is set (both is refused). A socket's peer must
// run as ADC_DAEMON_UID, by default the caller's own uid: the private
// socket admits only the daemon uid [M3]. The HTTPS listener's certificate
// must verify against exactly one of ADC_HTTPS_CA and ADC_HTTPS_PIN, never
// the system roots [R3]. The token comes from ADC_TOKEN or
// ~/.config/adc/token [R5], read for each command.
func ClientFromEnv(getenv func(string) string) Client {
	token := func() (identity.Secret, error) { return client.Token(getenv) }
	socket, addr := getenv(SocketEnv), getenv(HTTPSAddrEnv)
	switch {
	case addr == "" && (getenv(HTTPSCAEnv) != "" || getenv(HTTPSPinEnv) != ""):
		return failing{fmt.Errorf("%s and %s need %s", HTTPSCAEnv, HTTPSPinEnv, HTTPSAddrEnv)}
	case socket != "" && addr != "":
		return failing{fmt.Errorf("set one of %s and %s, not both", SocketEnv, HTTPSAddrEnv)}
	case addr != "":
		remote, err := client.NewRemote(addr, getenv(HTTPSCAEnv), getenv(HTTPSPinEnv))
		if err != nil {
			return failing{err}
		}
		return client.Transport{Remote: &remote, Token: token}
	case socket == "":
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
	return client.Transport{Socket: socket, DaemonUID: uid, Token: token}
}

// failing is the Client of a transport configured wrongly: every command
// fails with why, and commands that need no controller still run.
type failing struct{ err error }

func (f failing) Command(context.Context, string, []byte) ([]byte, error) { return nil, f.err }

func (f failing) Bootstrap(context.Context, []byte) ([]byte, error) { return nil, f.err }
