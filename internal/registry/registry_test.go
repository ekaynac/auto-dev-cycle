// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
)

const fixtures = "../../fixtures/m1/config/"

func load(t *testing.T, name string) types.ExecutorRegistry {
	t.Helper()
	doc, err := Load(fixtures + name)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func groups(t *testing.T) []types.ResourceGroup {
	t.Helper()
	raw, err := os.ReadFile(fixtures + "groups.json")
	var gs []types.ResourceGroup
	if err == nil {
		err = json.Unmarshal(raw, &gs)
	}
	if err != nil {
		t.Fatal(err)
	}
	return gs
}

// clone is a deep copy, so a case's edit reaches no other case.
func clone[T any](t *testing.T, v T) T {
	t.Helper()
	raw, err := json.Marshal(v)
	var out T
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func refusedBy(err error) Rule {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Rule
	}
	return ""
}

func TestFixtures(t *testing.T) {
	gs := groups(t)
	if _, err := capacity.NewReservations(gs); err != nil {
		t.Fatalf("the groups fixture: %v", err)
	}
	for name, want := range map[string]map[types.ExecutorID]string{
		"registry-stub.json":         {"exe_01m4tm99haemqt8es2qfbwj8pa": "stub-builder", "exe_01m4tm99haemr8ajyf09v4ytzb": "stub-reviewer"},
		"registry-live.example.json": {"exe_01m4tm99haemrs8zjs56wjm467": "local-coder", "exe_01m4tm99haemsbebz0rbxsgv6r": "api-reviewer"},
	} {
		r, err := New(load(t, name), gs)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for id, family := range want {
			if got := r.Family(id); got != family {
				t.Errorf("%s: Family(%s) = %q, want %q", name, id, got, family)
			}
			if e, ok := r.Executor(id); !ok || e.ID != id {
				t.Errorf("%s: Executor(%s) = %v, %v", name, id, e, ok)
			}
		}
	}
}

// F: an unknown family claimed independent. An executor the registry does
// not hold has the unknown family, which no roster admits; a registry that
// names an unknown family is refused at start.
func TestUnknownFamilyIsNeverIndependent(t *testing.T) {
	r, err := New(load(t, "registry-stub.json"), groups(t))
	if err != nil {
		t.Fatal(err)
	}
	const stranger = types.ExecutorID("exe_01m4tm99haemsbebz0rbxsgv6r") // the live example's, not the stub's
	if got := r.Family(stranger); got != receipt.UnknownFamily {
		t.Fatalf("Family(unregistered) = %q", got)
	}
	if _, ok := r.Executor(stranger); ok {
		t.Fatal("an unregistered executor was found")
	}
	if _, err := graph.Roster([]types.ExecutorID{"exe_01m4tm99haemr8ajyf09v4ytzb"}, r); err != nil {
		t.Fatalf("a registered reviewer's roster (positive control): %v", err)
	}
	if _, err := graph.Roster([]types.ExecutorID{stranger}, r); err == nil {
		t.Fatal("an unregistered executor's roster was admitted")
	}
	doc := clone(t, load(t, "registry-stub.json"))
	doc.Executors[1].Family = receipt.UnknownFamily
	if _, err := New(doc, groups(t)); refusedBy(err) != RuleUnknownFamily {
		t.Fatalf("a registered unknown family: %v", err)
	}
}

// F: a widening route, and the families rule. Each case changes one field
// of the stub registry (or the groups) and must be refused by its rule.
func TestNewRefuses(t *testing.T) {
	base, gs := load(t, "registry-stub.json"), groups(t)
	if _, err := New(base, gs); err != nil {
		t.Fatalf("the unedited registry (positive control): %v", err)
	}
	reviewer := func(f func(e *types.RegistryExecutor)) func(*types.ExecutorRegistry, []types.ResourceGroup) {
		return func(d *types.ExecutorRegistry, _ []types.ResourceGroup) { f(&d.Executors[1]) }
	}
	// every edits both executors, so their shared destination stays one
	// and only the rule under test can refuse.
	every := func(f func(e *types.RegistryDestination)) func(*types.ExecutorRegistry, []types.ResourceGroup) {
		return func(d *types.ExecutorRegistry, _ []types.ResourceGroup) {
			for i := range d.Executors {
				f(&d.Executors[i].Destination)
			}
		}
	}
	endpoint := func(u string) func(*types.ExecutorRegistry, []types.ResourceGroup) {
		return every(func(d *types.RegistryDestination) { d.Endpoint = u })
	}
	stubGroup := func(f func(g *types.ResourceGroup)) func(*types.ExecutorRegistry, []types.ResourceGroup) {
		return func(_ *types.ExecutorRegistry, g []types.ResourceGroup) { f(&g[0]) }
	}
	for name, c := range map[string]struct {
		edit func(*types.ExecutorRegistry, []types.ResourceGroup)
		want Rule
	}{
		"schema-invalid family": {reviewer(func(e *types.RegistryExecutor) { e.Family = "Stub-Builder" }), RuleUnreadable},
		"same family":           {reviewer(func(e *types.RegistryExecutor) { e.Family = "stub-builder" }), RuleSameFamily},
		"no reviewer":           {reviewer(func(e *types.RegistryExecutor) { e.Role = types.RegistryExecutorRoleBuilder }), RuleRoles},
		"no builder": {func(d *types.ExecutorRegistry, _ []types.ResourceGroup) {
			d.Executors[0].Role = types.RegistryExecutorRoleReviewer
		}, RuleRoles},
		"duplicate id":            {reviewer(func(e *types.RegistryExecutor) { e.ID = "exe_01m4tm99haemqt8es2qfbwj8pa" }), RuleDuplicate},
		"another adapter":         {reviewer(func(e *types.RegistryExecutor) { e.Adapter = "anthropic-messages" }), RuleAdapter},
		"broker":                  {every(func(d *types.RegistryDestination) { d.Kind = types.RegistryDestinationKindBroker }), RuleKind},
		"governed upstream":       {every(func(d *types.RegistryDestination) { d.Kind = types.RegistryDestinationKindGovernedUpstream }), RuleKind},
		"attested":                {every(func(d *types.RegistryDestination) { d.Verification = types.RegistryDestinationVerificationAttested }), RuleVerification},
		"reported":                {every(func(d *types.RegistryDestination) { d.Verification = types.RegistryDestinationVerificationReported }), RuleVerification},
		"external own":            {every(func(d *types.RegistryDestination) { d.TrustClass = types.RegistryDestinationTrustClassOwn }), RuleTrustClass},
		"one destination, two":    {reviewer(func(e *types.RegistryExecutor) { e.Destination.RetentionDays = nil }), RuleDestination},
		"plain http off loopback": {endpoint("http://api.example.com/v1"), RuleEndpoint},
		"plain http to a name":    {endpoint("http://localhost:18080/v1"), RuleEndpoint},
		"credential in the URL":   {endpoint("https://key:secret@api.example.com/v1"), RuleEndpoint},
		"query":                   {endpoint("https://api.example.com/v1?key=secret"), RuleEndpoint},
		"fragment":                {endpoint("https://api.example.com/v1#x"), RuleEndpoint},
		"another scheme":          {endpoint("ftp://api.example.com/v1"), RuleEndpoint},
		"relative":                {endpoint("/v1"), RuleEndpoint},
		"direct, no credential":   {reviewer(func(e *types.RegistryExecutor) { e.CredentialRef = nil }), RuleCredential},
		"cash, no price":          {reviewer(func(e *types.RegistryExecutor) { e.Price = nil }), RulePriceBasis},
		"free, priced":            {reviewer(func(e *types.RegistryExecutor) { e.CostBasis = types.RegistryExecutorCostBasisFree }), RulePriceBasis},
		"direct, local":           {reviewer(func(e *types.RegistryExecutor) { e.CostBasis, e.Price = types.RegistryExecutorCostBasisLocal, nil }), RulePriceBasis},
		"a group not listing it":  {reviewer(func(e *types.RegistryExecutor) { e.Group = "rg_01m4tm99haemtbemfkdve21grg" }), RuleGroup},
		"not a member":            {stubGroup(func(g *types.ResourceGroup) { g.Members = g.Members[:1] }), RuleGroup},
		"member of another group": {func(_ *types.ExecutorRegistry, g []types.ResourceGroup) {
			g[2].Members = append(g[2].Members, "exe_01m4tm99haemr8ajyf09v4ytzb")
		}, RuleGroup},
		"no groups": {func(*types.ExecutorRegistry, []types.ResourceGroup) {}, RuleGroup},
		"direct on slots": {stubGroup(func(g *types.ResourceGroup) {
			g.Kind, g.CapacityModel.Unit = types.ResourceGroupKindConcurrency, types.CapacityUnitSlots
		}), RuleGroupKind},
		"direct in requests": {stubGroup(func(g *types.ResourceGroup) { g.CapacityModel.Unit = types.CapacityUnitRequests }), RuleGroupKind},
		"unknown quota":      {stubGroup(func(g *types.ResourceGroup) { g.CapacityModel.QuotaState, g.CapacityModel.Limit = "unknown", nil }), RuleGroupKind},
		"window reset":       {stubGroup(func(g *types.ResourceGroup) { g.ReleaseRule = types.ResourceGroupReleaseRuleOnWindowReset }), RuleGroupKind},
		"a broker balance group": {stubGroup(func(g *types.ResourceGroup) {
			g.Kind, g.CapacityModel.Unit = types.ResourceGroupKindBrokerBalance, types.CapacityUnitUsdMicros
		}), RuleGroupKind},
		"a second destination": {reviewer(func(e *types.RegistryExecutor) { e.Destination.ID = "other" }), ""},
	} {
		doc, g := clone(t, base), clone(t, gs)
		c.edit(&doc, g)
		if name == "no groups" {
			g = nil
		}
		_, err := New(doc, g)
		if got := refusedBy(err); got != c.want || (c.want == "") != (err == nil) {
			t.Errorf("%s: refused by %q (%v), want %q", name, got, err, c.want)
		}
	}
}

// A local endpoint draws from a concurrency group in slots, known, at no
// cost; a direct API is never own and never local.
func TestLocalEndpoint(t *testing.T) {
	base, gs := load(t, "registry-live.example.json"), groups(t)
	for name, c := range map[string]struct {
		edit func(*types.RegistryExecutor, []types.ResourceGroup)
		want Rule
	}{
		"as given": {func(*types.RegistryExecutor, []types.ResourceGroup) {}, ""},
		"plain http on ::1": {func(e *types.RegistryExecutor, _ []types.ResourceGroup) {
			e.Destination.Endpoint = "http://[::1]:11434/v1"
		}, ""},
		"priced": {func(e *types.RegistryExecutor, _ []types.ResourceGroup) {
			e.CostBasis = types.RegistryExecutorCostBasisCash
		}, RulePriceBasis},
		"free": {func(e *types.RegistryExecutor, _ []types.ResourceGroup) {
			e.CostBasis = types.RegistryExecutorCostBasisFree
		}, RulePriceBasis},
		"estimated slots": {func(_ *types.RegistryExecutor, g []types.ResourceGroup) { g[1].CapacityModel.QuotaState = "estimated" }, RuleGroupKind},
		"on an account": {func(e *types.RegistryExecutor, g []types.ResourceGroup) {
			g[1].Kind, g[1].CapacityModel.Unit = "account_quota", "tokens"
		}, RuleGroupKind},
		// Only the kind rule stands between these and a local route.
		"a broker, as local": {func(e *types.RegistryExecutor, _ []types.ResourceGroup) {
			e.Destination.Kind = types.RegistryDestinationKindBroker
		}, RuleKind},
		"a governed upstream, as local": {func(e *types.RegistryExecutor, _ []types.ResourceGroup) {
			e.Destination.Kind = types.RegistryDestinationKindGovernedUpstream
		}, RuleKind},
		"plain http on LAN": {func(e *types.RegistryExecutor, _ []types.ResourceGroup) {
			e.Destination.Endpoint = "http://192.168.1.5:11434/v1"
		}, RuleEndpoint},
	} {
		doc, g := clone(t, base), clone(t, gs)
		c.edit(&doc.Executors[0], g)
		_, err := New(doc, g)
		if got := refusedBy(err); got != c.want || (c.want == "") != (err == nil) {
			t.Errorf("%s: refused by %q (%v), want %q", name, got, err, c.want)
		}
	}
}

func TestAdmits(t *testing.T) {
	r, err := New(load(t, "registry-live.example.json"), groups(t))
	if err != nil {
		t.Fatal(err)
	}
	const local, api = types.ExecutorID("exe_01m4tm99haemrs8zjs56wjm467"), types.ExecutorID("exe_01m4tm99haemsbebz0rbxsgv6r")
	both := []types.Slug{"local", "api"}
	for name, c := range map[string]struct {
		floor   types.ProjectProfileDataPolicyFloor
		allowed []types.Slug
		id      types.ExecutorID
		want    Rule
	}{
		"own floor, own destination":        {"own", both, local, ""},
		"own floor, contracted destination": {"own", both, api, RuleFloor},
		"contracted floor, contracted":      {"contracted", both, api, ""},
		"contracted floor, own":             {"contracted", both, local, ""},
		"free_unknown floor":                {"free_unknown", both, api, ""},
		"not allowed":                       {"free_unknown", []types.Slug{"local"}, api, RuleNotAllowed},
		"derived from a usage profile":      {"free_unknown", []types.Slug{}, api, RuleUsageProfile},
		"unregistered":                      {"free_unknown", both, "exe_01m4tm99haemqt8es2qfbwj8pa", RuleNotRegistered},
	} {
		dp := types.ProjectProfileDataPolicy{Floor: c.floor, Routing: types.ProjectProfileDataPolicyRoutingVerified, AllowedDestinations: c.allowed}
		err := r.Admits(dp, c.id)
		if got := refusedBy(err); got != c.want || (c.want == "") != (err == nil) {
			t.Errorf("%s: refused by %q (%v), want %q", name, got, err, c.want)
		}
	}
}

func TestLoadRefuses(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good, err := os.ReadFile(fixtures + "registry-stub.json")
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(write("good.json", string(good)), link); err != nil {
		t.Fatal(err)
	}
	extra := strings.Replace(string(good), `"schema"`, `"note": "x", "schema"`, 1)
	for name, path := range map[string]string{
		"absent":        filepath.Join(dir, "absent.json"),
		"a symlink":     link,
		"not JSON":      write("bad.json", "{"),
		"unknown field": write("extra.json", extra),
		"oversized":     write("big.json", string(good)+strings.Repeat(" ", maxFile)),
	} {
		_, err := Load(path)
		if refusedBy(err) != RuleUnreadable || strings.Contains(err.Error(), dir) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
