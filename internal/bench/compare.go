// SPDX-License-Identifier: Apache-2.0

package bench

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
)

// Delta is one metric of a task (and stage) that grew from an earlier
// run, or a task whose outcome changed. Compare reports; it never gates.
type Delta struct {
	Task    types.TaskID
	Stage   types.BenchmarkStageStage // "" for the task's own metrics
	Metric  string
	Was, Is string
}

// Compare is the deltas of cur against prev: per task present in both, its
// outcome or hold when it changed, its wall time and rescues when they
// grew, and per stage each metric that grew, charge and outstanding per
// basis and currency; a stage new in cur is reported against zero. Tasks only in one run are listed by Metric "task".
func Compare(prev, cur types.BenchmarkRun) []Delta {
	var out []Delta
	was := map[types.TaskID]types.BenchmarkTask{}
	for _, t := range prev.Tasks {
		was[t.Task] = t
	}
	seen := map[types.TaskID]bool{}
	for _, t := range cur.Tasks {
		seen[t.Task] = true
		p, ok := was[t.Task]
		if !ok {
			out = append(out, Delta{t.Task, "", "task", "absent", "present"})
			continue
		}
		if a, b := verdict(p), verdict(t); a != b {
			out = append(out, Delta{t.Task, "", "outcome", a, b})
		}
		if p.WallMs != nil && t.WallMs != nil && *t.WallMs > *p.WallMs {
			out = append(out, grew(t.Task, "", "wall_ms", *p.WallMs, *t.WallMs))
		}
		if t.Rescues > p.Rescues {
			out = append(out, grew(t.Task, "", "rescues", p.Rescues, t.Rescues))
		}
		ps := map[types.BenchmarkStageStage]types.BenchmarkStage{}
		for _, s := range p.Stages {
			ps[s.Stage] = s
		}
		for _, s := range t.Stages {
			q := ps[s.Stage]
			for _, m := range []struct {
				name    string
				was, is int64
			}{
				{"wall_ms", q.WallMs, s.WallMs}, {"unfinished", q.Unfinished, s.Unfinished}, {"attempts", q.Attempts, s.Attempts}, {"rounds", q.Rounds, s.Rounds},
				{"tokens.in", q.Tokens.In, s.Tokens.In}, {"tokens.out", q.Tokens.Out, s.Tokens.Out},
				{"tokens.cache_read", q.Tokens.CacheRead, s.Tokens.CacheRead}, {"tokens.cache_write", q.Tokens.CacheWrite, s.Tokens.CacheWrite},
				{"interventions", interventions(q), interventions(s)},
			} {
				if m.is > m.was {
					out = append(out, grew(t.Task, s.Stage, m.name, m.was, m.is))
				}
			}
			out = append(out, amountsGrew(t.Task, s.Stage, "charge_micros", q.Charge, s.Charge)...)
			out = append(out, amountsGrew(t.Task, s.Stage, "outstanding_micros", q.Outstanding, s.Outstanding)...)
		}
	}
	for _, t := range prev.Tasks {
		if !seen[t.Task] {
			out = append(out, Delta{t.Task, "", "task", "present", "absent"})
		}
	}
	return out
}

func grew(t types.TaskID, s types.BenchmarkStageStage, metric string, was, is int64) Delta {
	return Delta{t, s, metric, fmt.Sprint(was), fmt.Sprint(is)}
}

// verdict is a task's closure outcome and reason, or its hold.
func verdict(t types.BenchmarkTask) string {
	switch {
	case t.Outcome != nil && t.Reason != nil:
		return string(*t.Outcome) + "/" + t.Reason.String()
	case t.Hold != nil:
		return "held: " + *t.Hold
	}
	return "open"
}

// amountsGrew is a delta per basis and currency whose amount grew, so a
// rise in one currency never hides behind a fall in another.
func amountsGrew(t types.TaskID, s types.BenchmarkStageStage, metric string, was, is []types.BenchmarkAmount) []Delta {
	w := map[string]int64{}
	for _, a := range was {
		w[string(a.Basis)+" "+a.Currency] = a.AmountMicros
	}
	var out []Delta
	for _, a := range is {
		if k := string(a.Basis) + " " + a.Currency; a.AmountMicros > w[k] {
			out = append(out, grew(t, s, metric+" "+k, w[k], a.AmountMicros))
		}
	}
	return out
}

func interventions(s types.BenchmarkStage) int64 {
	return s.Interventions.Approvals + s.Interventions.Answers + s.Interventions.RailRaises
}

// Summary is the Markdown summary of run an exit record carries: per task
// its verdict and wall time, and per stage its wall time, unfinished nodes,
// tokens, sources, charge, outstanding liabilities, attempts, rounds and
// interventions; the task's rescues apart.
func Summary(run types.BenchmarkRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Benchmark (%s), checkpoint (%s), %s, build %s.\n\n", schema, run.Checkpoint, run.Arch, run.BuildCommit)
	b.WriteString("| Task | Stage | Wall (s) | Unfinished | Tokens in / out / cache read / cache write | Sources | Charge | Outstanding | Attempts | Rounds | Approvals / answers / rail raises |\n" +
		"|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, t := range run.Tasks {
		wall := "–"
		if t.WallMs != nil {
			wall = seconds(*t.WallMs)
		}
		fmt.Fprintf(&b, "| `%s` | **%s** | %s | | | | | | | | |\n", t.Task, cell(verdict(t)), wall)
		for _, s := range t.Stages {
			var src []string
			for _, x := range s.Sources {
				src = append(src, string(x))
			}
			fmt.Fprintf(&b, "| | %s | %s | %d | %d / %d / %d / %d | %s | %s | %s | %d | %d | %d / %d / %d |\n", s.Stage, seconds(s.WallMs), s.Unfinished,
				s.Tokens.In, s.Tokens.Out, s.Tokens.CacheRead, s.Tokens.CacheWrite, cmp.Or(strings.Join(src, ", "), "–"), money(s.Charge), money(s.Outstanding),
				s.Attempts, s.Rounds, s.Interventions.Approvals, s.Interventions.Answers, s.Interventions.RailRaises)
		}
		if t.Rescues > 0 {
			fmt.Fprintf(&b, "| | rescues | %d | | | | | | | | |\n", t.Rescues)
		}
	}
	return b.String()
}

// Deltas is the Markdown table of Compare's deltas an exit record carries.
func Deltas(ds []Delta) string {
	if len(ds) == 0 {
		return "No metric grew and no outcome changed.\n"
	}
	var b strings.Builder
	b.WriteString("| Task | Stage | Metric | Was | Is |\n|---|---|---|---|---|\n")
	for _, d := range ds {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", d.Task, cmp.Or(string(d.Stage), "–"), d.Metric, cell(d.Was), cell(d.Is))
	}
	return b.String()
}

// Path is run's file from the repository root:
// benchmarks/m1/<date>-<GOOS>-<GOARCH>-<checkpoint>.json.
func Path(run types.BenchmarkRun) string {
	return fmt.Sprintf("benchmarks/m1/%s-%s-%s.json", string(run.RecordedAt)[:min(10, len(run.RecordedAt))], strings.ReplaceAll(run.Arch, "/", "-"), run.Checkpoint)
}

// cell escapes a Markdown table cell.
func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func seconds(ms int64) string { return fmt.Sprintf("%d.%03d", ms/1000, ms%1000) }

func money(as []types.BenchmarkAmount) string {
	if len(as) == 0 {
		return "–"
	}
	var parts []string
	for _, a := range as {
		parts = append(parts, fmt.Sprintf("%d.%06d %s %s (%d calls)", a.AmountMicros/1_000_000, a.AmountMicros%1_000_000, strings.TrimSpace(a.Currency), a.Basis, a.Calls))
	}
	return strings.Join(parts, "; ")
}
