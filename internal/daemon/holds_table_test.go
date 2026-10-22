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
)

// #226 entry 156: every hold adcd's loop or runner raises has a clearing
// path, or a stated terminal state. A typed scan of internal/daemon,
// internal/dispatch and internal/reconcile finds every raise site (a call of
// graph.Raise, Retry, Probe or Exhaust, or of daemon's raise) and every hold
// built for one: a types.NodeHold literal, and a call of dispatch's held or
// configRefusal or of graph.RailHold, each with its kind and its signal's
// expression (a local variable: each value assigned to it). Both must be
// exactly the tables below, so a new raise site, or a new hold, without a
// row fails; each row names the function that clears it, which must call
// graph.Clear or graph.ClearRail, or states why nothing does.

const mod = "github.com/ekaynac/auto-dev-cycle/internal/"

// raiseSites are the functions that raise a node hold.
var raiseSites = []string{"daemon.placer.Place", "daemon.runner.holdOn", "dispatch.Dispatcher.Launch"}

// holdRow is a hold built for a raise site: where, its kind and its
// signal's expression, and its clearing path (clearer, a function calling
// graph.Clear or ClearRail, with when) or its stated terminal state.
type holdRow struct {
	at, kind, signal string
	clearer, when    string
}

var holdRows = []holdRow{
	// The placer's configuration holds (placer.Place, from Match's refusals).
	{"daemon.placer.Place", "source_invalid_config", "dispatch.PlacementSignal(cfg.signal)", "daemon.placer.Unblocked", "the card is made and Configured passes"},
	{"dispatch.heldOn", "source_invalid_config", `"executor " + string(executor)`, "daemon.placer.Unblocked", "Configured passes (placement: prefixed by configRefusal)"},
	{"dispatch.heldOn", "source_invalid_config", `"profile " + string(card.Project)`, "daemon.placer.Unblocked", "Configured passes"},
	{"dispatch.Dispatcher.footprint", "source_invalid_config", `"executor " + string(executor)`, "daemon.placer.Unblocked", "Configured's footprint check passes"},
	{"dispatch.Dispatcher.usdRail", "source_invalid_config", `"profile " + string(card.Project)`, "daemon.placer.Unblocked", "Configured: the profile has USD rails"},
	{"dispatch.Dispatcher.reservation", "budget_rail", "b.fit(e, graph.RailTaskUSD, graph.RailDayUSD)", "authority.TaskRaiseRailHandler.Logic",
		"rail:per_task_usd: task.raise_rail (graph.ClearRail); rail:per_day_usd: daemon.placer.Reopened, the day's room (RoomToday)"},
	{"dispatch.Dispatcher.usdRail", "budget_rail", "r.rail", "daemon.placer.Reopened",
		"rail:per_day_usd: the day's room (RoomToday); rail:per_task_usd: task.raise_rail (graph.ClearRail)"},
	// The launch's misses (Dispatcher.Launch, #226 entry 156).
	{"dispatch.Dispatcher.miss", "reviewer_unavailable", `"registry:" + string(ni.Executor)`, "daemon.reviews.Unheld", "a review node's roster is the registry's again, or re-derived"},
	{"dispatch.Dispatcher.miss", "source_invalid_config", `PlacementSignal("executor " + string(l.Executor))`, "daemon.placer.Unblocked", "Configured re-derives the placement"},
	{"dispatch.Dispatcher.miss", "wrong_owner", "imageSignal(card.Image)", "daemon.placer.Unblocked", "the project's pinned image resolves (placer.imageReady)"},
	{"dispatch.Dispatcher.miss", "source_invalid_config", `PlacementSignal("destination " + string(e.Destination.ID))`, "daemon.placer.Unblocked", "Configured: the credential is held"},
	{"dispatch.Dispatcher.attemptRail", "budget_rail", "graph.RailAttempts", "authority.TaskRaiseRailHandler.Logic", "task.raise_rail of attempts_per_round"},
	// The runner's holds (runner.holdOn): Unblocked asks runner.cleared.
	{"daemon.runner.holdOn", "source_invalid_config", "runnerDirs", "daemon.placer.Unblocked", "the runner's directories are directories (dirsReady)"},
	{"daemon.runner.holdOn", "source_invalid_config", "runnerImage", "daemon.placer.Unblocked", "the pinned image resolves (placer.imageReady)"},
	{"daemon.runner.holdOn", "source_invalid_config", "r.retrySignal(tx, node)", "daemon.placer.Unblocked", "\"retry n until t\": t has come; past maxTries, runnerFailed"},
	{"daemon.runner.holdOn", "source_invalid_config", `reconcile.RunnerSignal("profile " + string(d))`, "daemon.placer.Unblocked", "the project's profile changed"},
	{"daemon.runner.holdOn", "source_invalid_config", "runnerFailed", "", "terminal, stated (holdOn): nothing clears it; revoking the task's approval abandons it"},
}

func TestEveryRaisedHoldHasAClearingPath(t *testing.T) {
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo, Dir: "."}
	pkgs, err := packages.Load(cfg, "./", "../dispatch", "../reconcile", "../authority")
	if err != nil || packages.PrintErrors(pkgs) > 0 {
		t.Fatalf("loading: %v", err)
	}
	var sites []string
	var built []holdRow
	clearers := map[string]bool{} // the functions that call graph.Clear or graph.ClearRail
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				name := funcName(p.Name, fn)
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.CallExpr:
						switch callee(p.TypesInfo, x.Fun) {
						case "graph.Raise", "graph.Retry", "graph.Probe", "graph.Exhaust", "daemon.raise":
							sites = append(sites, name)
						case "graph.Clear", "graph.ClearRail":
							clearers[name] = true
						case "dispatch.held":
							if p.Name == "dispatch" && name != "dispatch.configRefusal" {
								built = append(built, built1(p, fn, name, x.Args[1], x.Args[2])...)
							}
						case "dispatch.configRefusal":
							built = append(built, built1(p, fn, name, nil, x.Args[1])...)
						case "graph.RailHold":
							built = append(built, built1(p, fn, name, nil, x.Args[0])...)
						}
					case *ast.CompositeLit:
						if tv, ok := p.TypesInfo.Types[x]; ok && types.TypeString(tv.Type, nil) == "github.com/ekaynac/auto-dev-cycle/gen/go/types.NodeHold" && name != "dispatch.held" {
							var kind, signal ast.Expr
							for _, e := range x.Elts {
								if kv, ok := e.(*ast.KeyValueExpr); ok {
									switch kv.Key.(*ast.Ident).Name {
									case "Hold":
										kind = kv.Value
									case "SignalInstance":
										signal = kv.Value
									}
								}
							}
							built = append(built, built1(p, fn, name, kind, signal)...)
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
			t.Errorf("%s: its clearer %s calls neither graph.Clear nor graph.ClearRail", key(r), r.clearer)
		}
	}
	for _, r := range built {
		got = append(got, key(r))
	}
	slices.Sort(want)
	slices.Sort(got)
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

// built1 is the holds built in fn (named name) of kind (nil: a rail's,
// budget_rail; or source_invalid_config for configRefusal, by name), on
// signal: one per value a local variable signal is assigned.
func built1(p *packages.Package, fn *ast.FuncDecl, name string, kind, signal ast.Expr) []holdRow {
	k := "budget_rail"
	switch {
	case kind != nil:
		k = constant.StringVal(p.TypesInfo.Types[kind].Value)
	case signal != nil && callee(p.TypesInfo, outer(fn, signal)) == "dispatch.configRefusal":
		k = "source_invalid_config"
	}
	var out []holdRow
	for _, s := range values(p, fn, signal) {
		out = append(out, holdRow{at: name, kind: k, signal: s})
	}
	return out
}

// outer is the call whose argument e is, in fn.
func outer(fn *ast.FuncDecl, e ast.Expr) ast.Expr {
	var fun ast.Expr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && slices.Contains(c.Args, e) {
			fun = c.Fun
		}
		return fun == nil
	})
	return fun
}

// values is e's source, or, for a local variable, each value fn assigns it.
func values(p *packages.Package, fn *ast.FuncDecl, e ast.Expr) []string {
	id, ok := e.(*ast.Ident)
	obj, _ := p.TypesInfo.Uses[id].(*types.Var)
	if !ok || obj == nil || obj.Parent() == obj.Pkg().Scope() {
		return []string{types.ExprString(e)}
	}
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
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
