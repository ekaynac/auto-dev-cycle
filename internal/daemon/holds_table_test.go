// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"go/ast"
	"go/constant"
	"go/types"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/ekaynac/auto-dev-cycle/internal/reconcile"
)

// #226 entry 156: every hold adcd's loop or runner raises has a clearing
// path, or a stated terminal state. A typed scan of internal/daemon,
// dispatch, reconcile and graph finds every raise site (a call of graph's
// Raise, raise or retry, or of daemon's raise) and every hold built for one:
// a types.NodeHold literal, and a call of dispatch's held or configRefusal or
// of graph.RailHold, each with its kind and its signal's expression (a local
// variable: each value assigned to it), in a function or a function literal
// a package-level var holds (Opus #400 M1). Both must be exactly the tables
// below, so a new raise site, or a new hold, without a row fails. Each row
// names the function that clears it, which must call graph.Clear, ClearRail
// or cleared, or states why nothing does; a row whose clearer the loop
// drives (Unblocked, Reopened, Unheld) gives a sample signal, which the
// loop's own rule (reconcile.Offered) must offer to it. Not seen: a signal
// set on a hold after its literal (graph.Probe's on its caller's hold), and
// holds built outside these packages (the M0 fixtures' callers of Retry,
// Probe and Exhaust).

const mod = "github.com/ekaynac/auto-dev-cycle/internal/"

// raiseSites are the functions that raise a node hold.
var raiseSites = []string{"daemon.placer.Place", "daemon.runner.holdOn", "daemon.runner.notIndependent", "dispatch.Dispatcher.Launch", "graph.Exhaust", "graph.Probe", "graph.Raise",
	"graph.Retry", "graph.evaluation.await", "graph.evaluation.claim", "graph.evaluation.holdReceipt", "graph.evaluation.roundRail", "graph.mergeRail", "graph.retry"}

// holdRow is a hold built for a raise site: where, its kind and its
// signal's expression; a sample signal; and its clearing path (clearer, a
// function calling graph.Clear, ClearRail or cleared, with when) or its
// stated terminal state.
type holdRow struct {
	at, kind, signal, sample string
	clearer, when            string
}

// retryWait is the kind of a hold graph.retry raises: its result's reason's.
const retryWait = "(the result's reason's)"

var holdRows = []holdRow{
	// The placer's configuration holds (placer.Place, from Match's refusals).
	{"daemon.placer.Place", "source_invalid_config", "dispatch.PlacementSignal(cfg.signal)", "placement: profile p", "daemon.placer.Unblocked", "the card is made and Configured passes"},
	{"dispatch.heldOn", "source_invalid_config", `"executor " + string(executor)`, "placement: executor e", "daemon.placer.Unblocked", "Configured passes (configRefusal prefixes placement: )"},
	{"dispatch.heldOn", "source_invalid_config", `"profile " + string(card.Project)`, "placement: profile p", "daemon.placer.Unblocked", "Configured passes"},
	{"dispatch.Dispatcher.footprint", "source_invalid_config", `"executor " + string(executor)`, "placement: executor e", "daemon.placer.Unblocked", "Configured's footprint check passes"},
	{"dispatch.Dispatcher.usdRail", "source_invalid_config", `"profile " + string(card.Project)`, "placement: profile p", "daemon.placer.Unblocked", "Configured: the profile has USD rails"},
	{"dispatch.Dispatcher.reservation", "budget_rail", "b.fit(e, graph.RailTaskUSD, graph.RailDayUSD)", "rail:per_task_usd", "authority.TaskRaiseRailHandler.Logic",
		"rail:per_task_usd: task.raise_rail (graph.ClearRail); rail:per_day_usd: as usdRail's"},
	{"dispatch.Dispatcher.usdRail", "budget_rail", "r.rail", "rail:per_day_usd", "daemon.placer.Reopened",
		"rail:per_day_usd: the day's room (RoomToday); rail:per_task_usd: task.raise_rail (graph.ClearRail)"},
	// The launch's misses (Dispatcher.Launch, #226 entry 156).
	// rosterMiss: raised by Launch's miss and by Match's refusal (placer.Place; Astra chunk 3 H1).
	{"dispatch.rosterMiss", "reviewer_unavailable", `"registry:" + string(ni.Executor)`, "registry:e", "daemon.reviews.Unheld", "a review node's roster is the registry's again, or re-derived"},
	{"dispatch.Dispatcher.miss", "source_invalid_config", `PlacementSignal("executor " + string(l.Executor))`, "placement: executor e", "daemon.placer.Unblocked", "Configured re-derives the placement"},
	{"dispatch.Dispatcher.miss", "wrong_owner", "imageSignal(card.Image)", "image i", "daemon.placer.Unblocked", "the project's pinned image resolves (placer.imageReady)"},
	{"dispatch.Dispatcher.miss", "source_invalid_config", `PlacementSignal("destination " + string(e.Destination.ID))`, "placement: destination d", "daemon.placer.Unblocked", "Configured: the credential is held"},
	{"dispatch.Dispatcher.attemptRail", "budget_rail", "graph.RailAttempts", "rail:attempts_per_round", "authority.TaskRaiseRailHandler.Logic", "task.raise_rail of attempts_per_round"},
	// The runner's holds (runner.holdOn): Unblocked asks runner.cleared.
	{"daemon.runner.holdOn", "source_invalid_config", "runnerDirs", "runner: dirs", "daemon.placer.Unblocked", "the runner's directories are directories (dirsReady)"},
	{"daemon.runner.holdOn", "source_invalid_config", "runnerImage", "runner: image", "daemon.placer.Unblocked", "the pinned image resolves (placer.imageReady)"},
	{"daemon.runner.holdOn", "source_invalid_config", "r.retrySignal(tx, node)", "runner: retry 1 until t", "daemon.placer.Unblocked", "\"retry n until t\": t has come; past maxTries, runnerFailed"},
	{"daemon.runner.holdOn", "source_invalid_config", `reconcile.RunnerSignal("profile " + string(d))`, "runner: profile d", "daemon.placer.Unblocked", "the project's profile changed"},
	{"daemon.runner.holdOn", "source_invalid_config", "runnerFailed", "runner: failed", "", "terminal, stated (holdOn): nothing clears it; revoking the task's approval abandons it"},
	// A review's commit refused for independence (runner.notIndependent; P22a-6c).
	{"daemon.runner.notIndependent", "reviewer_unavailable", "ni.SignalInstance()", "registry:e", "daemon.reviews.Unheld", "an independent roster: cleared, or the review re-derived"},
	// The evaluator's and Operate's (graph; Opus #400 M1).
	{"graph.evaluation.await", "awaiting_input", "awaitSignal(ask)", "oi@1", "graph.evaluation.clear", "the answer in force arrives (input.answer)"},
	{"graph.evaluation.claim", "reviewer_unavailable", "notIndependent.SignalInstance()", "registry:e", "daemon.reviews.Unheld", "an independent roster: cleared, or the review re-derived"},
	{"graph.evaluation.holdReceipt", "awaiting_receipt", "receiptSignal(m)", "receipt:gate:receipt_incomplete", "graph.evaluation.receipts",
		"the input serves (cleared), or is refused for another reason (cleared and raised anew); P22a-6b"},
	{"graph.evaluation.roundRail", "budget_rail", "RailRounds", "rail:rounds_per_task", "authority.TaskRaiseRailHandler.Logic", "task.raise_rail of rounds_per_task"},
	{"graph.mergeRail", "budget_rail", "RailMergeOps", "rail:merge_operations_per_generation", "authority.TaskRaiseRailHandler.Logic", "task.raise_rail of merge_operations_per_generation"},
	{"graph.Exhaust", retryWait, `"caps:" + string(result)`, "caps:r", "graph.Invalidate", "a retry wait: retired when its node is invalidated (M0's evidence cap)"},
}

// offered are the clearers the reconcile loop drives, each offered a hold
// by reconcile.Offered.
var offered = []string{"daemon.placer.Unblocked", "daemon.placer.Reopened", "daemon.reviews.Unheld"}

func TestEveryRaisedHoldHasAClearingPath(t *testing.T) {
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo, Dir: "."}
	pkgs, err := packages.Load(cfg, "./", "../dispatch", "../reconcile", "../graph", "../authority")
	if err != nil || packages.PrintErrors(pkgs) > 0 {
		t.Fatalf("loading: %v", err)
	}
	var sites []string
	var built []holdRow
	clearers := map[string]bool{} // the functions that call graph.Clear, ClearRail or cleared
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			for name, body := range bodies(p.Name, f) {
				ast.Inspect(body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.CallExpr:
						switch callee(p.TypesInfo, x.Fun) {
						case "graph.Raise", "graph.raise", "graph.retry", "daemon.raise":
							sites = append(sites, name)
						case "graph.Clear", "graph.ClearRail", "graph.cleared":
							clearers[name] = true
						case "dispatch.held":
							if name != "dispatch.configRefusal" {
								built = append(built, builtOn(p, body, name, kindOf(p, x.Args[1]), x.Args[2])...)
							}
						case "dispatch.configRefusal":
							built = append(built, builtOn(p, body, name, "source_invalid_config", x.Args[1])...)
						case "graph.RailHold":
							built = append(built, builtOn(p, body, name, "budget_rail", x.Args[0])...)
						}
					case *ast.CompositeLit:
						if tv, ok := p.TypesInfo.Types[x]; ok && types.TypeString(tv.Type, nil) == mod[:len(mod)-len("internal/")]+"gen/go/types.NodeHold" &&
							name != "dispatch.held" && name != "graph.RailHold" {
							kind, signal := retryWait, ast.Expr(nil)
							for _, e := range x.Elts {
								if kv, ok := e.(*ast.KeyValueExpr); ok {
									switch kv.Key.(*ast.Ident).Name {
									case "Hold":
										kind = kindOf(p, kv.Value)
									case "SignalInstance":
										signal = kv.Value
									}
								}
							}
							built = append(built, builtOn(p, body, name, kind, signal)...)
						}
					}
					return true
				})
			}
		}
	}
	slices.Sort(sites)
	if sites = slices.Compact(sites); !slices.Equal(sites, raiseSites) {
		t.Errorf("the raise sites are %q; the table has %q: add each new one, and its holds' rows", sites, raiseSites)
	}
	key := func(r holdRow) string { return r.at + " | " + r.kind + " | " + r.signal }
	var want, got []string
	for _, r := range holdRows {
		want = append(want, key(r))
		switch {
		case r.clearer == "" && !strings.HasPrefix(r.when, "terminal, stated"):
			t.Errorf("%s: no clearing path and no stated terminal state (#226 entry 156)", key(r))
		case r.clearer != "" && !clearers[r.clearer]:
			t.Errorf("%s: its clearer %s calls none of graph.Clear, ClearRail and cleared", key(r), r.clearer)
		case slices.Contains(offered, r.clearer) && !reconcile.Offered(r.kind, r.sample):
			t.Errorf("%s: the loop never offers %s on %q to its clearer %s (reconcile.Offered)", key(r), r.kind, r.sample, r.clearer)
		}
	}
	for _, r := range built {
		got = append(got, key(r))
	}
	for _, k := range got {
		if !slices.Contains(want, k) {
			t.Errorf("a hold with no row: %s", k)
		}
	}
	for _, k := range want {
		if !slices.Contains(got, k) {
			t.Errorf("a row of no hold: %s", k)
		}
	}
}

// bodies are f's function bodies by name: each function's (pkg.Func,
// pkg.Recv.Method), and each function literal's a package-level var holds
// (pkg.var).
func bodies(pkg string, f *ast.File) map[string]*ast.BlockStmt {
	out := map[string]*ast.BlockStmt{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				out[funcName(pkg, d)] = d.Body
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				if v, ok := s.(*ast.ValueSpec); ok {
					for i, e := range v.Values {
						if fl, ok := e.(*ast.FuncLit); ok {
							out[pkg+"."+v.Names[i].Name] = fl.Body
						}
					}
				}
			}
		}
	}
	return out
}

// funcName is fn's name in package pkg: pkg.Func or pkg.Recv.Method.
func funcName(pkg string, fn *ast.FuncDecl) string {
	if fn.Recv == nil {
		return pkg + "." + fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	return pkg + "." + t.(*ast.Ident).Name + "." + fn.Name.Name
}

// callee is a call's function as pkg.Name, for a package-level function or
// variable of this module.
func callee(info *types.Info, fun ast.Expr) string {
	var id *ast.Ident
	switch f := fun.(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	default:
		return ""
	}
	obj := info.Uses[id]
	if obj == nil || obj.Pkg() == nil || !strings.HasPrefix(obj.Pkg().Path(), mod) || obj.Parent() != obj.Pkg().Scope() {
		return ""
	}
	return obj.Pkg().Name() + "." + obj.Name()
}

// kindOf is a hold kind's constant.
func kindOf(p *packages.Package, e ast.Expr) string {
	if v := p.TypesInfo.Types[e].Value; v != nil && v.Kind() == constant.String {
		return constant.StringVal(v)
	}
	return "(not a constant: " + types.ExprString(e) + ")"
}

// builtOn is the holds of kind built in body (named name) on signal: one per
// value a local variable signal is assigned.
func builtOn(p *packages.Package, body *ast.BlockStmt, name, kind string, signal ast.Expr) []holdRow {
	var out []holdRow
	for _, s := range values(p, body, signal) {
		out = append(out, holdRow{at: name, kind: kind, signal: s})
	}
	return out
}

// values is e's expression, or, for a local variable, each value body
// assigns it.
func values(p *packages.Package, body *ast.BlockStmt, e ast.Expr) []string {
	id, ok := e.(*ast.Ident)
	obj, _ := p.TypesInfo.Uses[id].(*types.Var)
	if !ok || obj == nil || obj.Parent() == obj.Pkg().Scope() {
		return []string{types.ExprString(e)}
	}
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		if a, ok := n.(*ast.AssignStmt); ok {
			for i, l := range a.Lhs {
				if li, ok := l.(*ast.Ident); ok && (p.TypesInfo.Defs[li] == obj || p.TypesInfo.Uses[li] == obj) {
					out = append(out, types.ExprString(a.Rhs[min(i, len(a.Rhs)-1)]))
				}
			}
		}
		return true
	})
	return out
}
