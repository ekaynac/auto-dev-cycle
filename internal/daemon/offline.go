// SPDX-License-Identifier: Apache-2.0

package daemon

// The offline recovery commands (04 "Bootstrap", 14 R7, R8). Each runs as
// the daemon uid while the daemon is stopped: it holds adcd.lock from
// before the store opens until after it closes, passes every startup trust
// check, and commits one event as the system actor through its claim,
// never through an endpoint. None prints or logs a secret.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Commands are adcd's offline commands, by their two words.
var Commands = map[[2]string]func([]string, io.Writer) int{
	{"bootstrap", "renew"}: RenewCommand,
}

const renewUsage = "usage: adcd bootstrap renew --state-dir DIR [--resource-groups FILE] [--trusted-head SEQ:sha256:HEX]"

var errSpent = &StartError{Outcome: vocab.Failed, Reason: vocab.ReasonConfirmationRefused, Detail: "the bootstrap is consumed or was never issued; it is not renewed"}

func noFlags(*flag.FlagSet) func(*Config) { return func(*Config) {} }

// RenewCommand is `adcd bootstrap renew` (14 R8).
func RenewCommand(args []string, stderr io.Writer) int {
	cfg, err := loadConfig("adcd bootstrap renew", renewUsage, args, noFlags)
	if err == nil {
		err = renewBootstrap(context.Background(), cfg)
	}
	if err == nil {
		fmt.Fprintln(stderr, "adcd: bootstrap renewed; the new one-time setup secret is bootstrap.secret in the state directory, live for 24h (adc admin bootstrap)")
	}
	return exitCode(err, stderr)
}

// offline runs fn on the store opened offline, then closes it before the
// lock is released.
func offline(ctx context.Context, cfg Config, fn func(*Daemon) error) error {
	d, err := start(ctx, cfg, true)
	if err != nil {
		return err
	}
	err = fn(d)
	if cerr := d.Close(); cerr != nil {
		return errors.Join(err, couldNotRun(vocab.ReasonStoreUnreadable, "the event store did not close cleanly", cerr))
	}
	return err
}

// renewBootstrap replaces an unconsumed bootstrap's secret with a fresh
// one, through the held state directory. Only this command renews.
func renewBootstrap(ctx context.Context, cfg Config) error {
	return offline(ctx, cfg, func(d *Daemon) error {
		var st identity.BootstrapState
		if err := d.store.View(ctx, func(tx *eventstore.Tx) (err error) {
			st, err = identity.BootstrapOf(tx)
			return err
		}); err != nil {
			return couldNotRun(vocab.ReasonStoreUnreadable, "the bootstrap state cannot be read", err)
		}
		if st.Consumed || st.Secret == nil {
			return errSpent
		}
		if err := d.boot.issue(ctx, true); err != nil {
			return couldNotRun(vocab.ReasonStoreUnreadable, "the bootstrap secret cannot be renewed", err)
		}
		return nil
	})
}
