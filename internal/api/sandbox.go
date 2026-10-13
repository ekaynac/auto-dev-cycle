// SPDX-License-Identifier: Apache-2.0

package api

// The sandbox probe (docs/design/m1/00-slice-plan.md P9): POST
// /v1/dev/sandbox-probe?image=REF@DIGEST&mode=gate|build runs the fixed
// probe in a fresh sandbox run, with no task or lease, for an operator
// only, one probe at a time. A build run gets a placeholder gateway
// socket. Every refusal's detail is fixed text; the cause goes to Log.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

const SandboxProbePath = "/v1/dev/sandbox-probe"

// ProbeBudget bounds a probe end to end (image inspection, the run, its
// removal); the answer's write deadline and a client's wait are longer.
const ProbeBudget = sandbox.ProbeTimeout + time.Minute

// SandboxProbe serves SandboxProbePath; Host nil is no sandbox configured.
type SandboxProbe struct {
	Store *eventstore.Store
	Gate  *Slots
	Host  *sandbox.Host
	Log   io.Writer
}

var probing sync.Mutex

func (h SandboxProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, err := Authenticate(h.Store, r, h.Gate)
	var a types.Actor
	if err == nil {
		err = h.Store.View(r.Context(), func(tx *eventstore.Tx) (err error) {
			a, err = p.Live(tx)
			return err
		})
	}
	q := r.URL.Query()
	mode := sandbox.Mode(q.Get("mode"))
	switch {
	case errors.Is(err, ErrBusy):
		refuseBusy(w)
	case errors.Is(err, identity.ErrUnauthenticated):
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
	case err != nil:
		h.refuse(w, err, vocab.ReasonStoreUnreadable, "the event store could not be read")
	case a.Kind != types.ActorKindOperator:
		writeResult(w, http.StatusForbidden, vocab.Failed, vocab.ReasonOperatorOnly, "only an operator probes the sandbox")
	case len(q) != 2 || len(q["image"]) != 1 || len(q["mode"]) != 1 || mode != sandbox.Gate && mode != sandbox.Build:
		http.Error(w, "the probe takes exactly image=REF@DIGEST and mode=gate|build", http.StatusBadRequest)
	case h.Host == nil:
		h.refuse(w, nil, vocab.ReasonPreconditionFailed, "no sandbox is configured (adcd run --sandbox-root)")
	case !probing.TryLock():
		refuseBusy(w)
	default:
		defer probing.Unlock()
		// The answer comes only after the probe: its write deadline is the
		// probe's budget with a margin, and the probe has its own deadline.
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(ProbeBudget + 30*time.Second)); err != nil {
			h.refuse(w, err, vocab.ReasonPreconditionFailed, "the probe's answer could not be given a deadline")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), ProbeBudget)
		defer cancel()
		h.probe(w, ctx, q.Get("image"), mode)
	}
}

func (h SandboxProbe) probe(w http.ResponseWriter, ctx context.Context, ref string, mode sandbox.Mode) {
	img, err := h.Host.Resolve(ctx, ref)
	if err != nil {
		h.refuse(w, err, vocab.ReasonPreconditionFailed, "the image is not present at its pinned, fully qualified digest")
		return
	}
	gateway := ""
	if mode == sandbox.Build { // a placeholder: the gateway (P11) is not wired yet
		dir, err := os.MkdirTemp("", "adc-gw")
		var ln net.Listener
		if err == nil {
			defer os.RemoveAll(dir)
			gateway = filepath.Join(dir, "gateway.sock")
			ln, err = net.Listen("unix", gateway)
		}
		if err != nil {
			h.refuse(w, err, vocab.ReasonPreconditionFailed, "the placeholder gateway socket could not be made")
			return
		}
		defer ln.Close()
	}
	report, err := h.Host.Probe(ctx, img, mode, gateway)
	var b []byte
	if err == nil {
		b, err = contract.Encode("api/sandbox-probe", report)
	}
	if err != nil {
		h.refuse(w, err, vocab.ReasonPreconditionFailed, "the probe did not run to its end; the controller log has the detail")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(b, '\n'))
}

func (h SandboxProbe) refuse(w http.ResponseWriter, err error, reason vocab.Reason, detail string) {
	if err != nil && h.Log != nil {
		fmt.Fprintf(h.Log, "adcd: sandbox probe: %v\n", err)
	}
	writeResult(w, http.StatusServiceUnavailable, vocab.CouldNotRun, reason, detail)
}
