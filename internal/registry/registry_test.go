// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/graph"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
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
	if mutation.Enabled { // -tags adcmutation: with the mutation on, the checks above must turn red
		t.Setenv("ADC_MUTATION", mutation.UnknownFamilyIndependent)
		if _, err := graph.Roster([]types.ExecutorID{stranger}, r); err != nil || r.Family(stranger) == receipt.UnknownFamily {
			t.Errorf("under %s an unregistered executor is still unknown (%v): the checks above would not see it", mutation.UnknownFamilyIndependent, err)
		}
		t.Setenv("ADC_MUTATION", "")
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
		"whitespace": {endpoint("https://api.example.com/v1 x"), RuleEndpoint},
		"backslash":  {endpoint(`https://api.example.com\v1`), RuleEndpoint},
		"a group mixing destinations": {reviewer(func(e *types.RegistryExecutor) {
			ref := types.Slug("other") // its own credential: only the group rule refuses
			e.Destination.ID, e.Destination.Endpoint, e.CredentialRef = "other", "http://127.0.0.2:18080/v1", &ref
		}), RuleGroup},
		// One account (destination and credential) split across groups would
		// draw its one limit twice.
		"one account, two groups": {func(d *types.ExecutorRegistry, g []types.ResourceGroup) {
			d.Executors[1].Group = g[2].ID
			g[0].Members, g[2].Members = g[0].Members[:1], []types.ExecutorID{d.Executors[1].ID}
		}, RuleGroup},
		"an unregistered member": {stubGroup(func(g *types.ResourceGroup) {
			g.Members = append(g.Members, "exe_01m4tm99haemrs8zjs56wjm467")
		}), RuleGroup},
		// One credential reference at two origins: one account's key would
		// be injected at another endpoint (Opus review of #248, M1).
		"one credential, two origins": {func(d *types.ExecutorRegistry, g []types.ResourceGroup) {
			d.Executors[1].Destination.ID, d.Executors[1].Destination.Endpoint = "other", "https://other.example/v1"
			d.Executors[1].Group, g[0].Members, g[2].Members = g[2].ID, g[0].Members[:1], []types.ExecutorID{d.Executors[1].ID}
		}, RuleCredentialRef},
		// The builder's model under another family name.
		"aliased family": {reviewer(func(e *types.RegistryExecutor) { e.Family, e.Model = "stub-other", "stub-builder-1" }), RuleSameFamily},
		// Relabelling: the same endpoint (origin) under a second destination id.
		"one endpoint, two destination ids": {func(d *types.ExecutorRegistry, g []types.ResourceGroup) {
			d.Executors[1].Destination.ID, d.Executors[1].Destination.Endpoint = "other", "HTTP://127.0.0.1:18080/v2/"
			d.Executors[1].Group, g[0].Members, g[2].Members = g[2].ID, g[0].Members[:1], []types.ExecutorID{d.Executors[1].ID}
		}, RuleDestination},
		// One IP literal in two spellings is one origin (#226 entry 41).
		"one endpoint, an IPv4-mapped literal": {func(d *types.ExecutorRegistry, g []types.ResourceGroup) {
			d.Executors[1].Destination.ID, d.Executors[1].Destination.Endpoint = "other", "http://[::ffff:127.0.0.1]:18080/v2"
			d.Executors[1].Group, g[0].Members, g[2].Members = g[2].ID, g[0].Members[:1], []types.ExecutorID{d.Executors[1].ID}
		}, RuleDestination},
		"one endpoint, two IPv6 spellings": {func(d *types.ExecutorRegistry, g []types.ResourceGroup) {
			d.Executors[0].Destination.Endpoint, d.Executors[1].Destination.Endpoint = "http://[::1]:18080/v1", "http://[0:0::1]:18080/v1"
			d.Executors[1].Destination.ID = "other"
			d.Executors[1].Group, g[0].Members, g[2].Members = g[2].ID, g[0].Members[:1], []types.ExecutorID{d.Executors[1].ID}
		}, RuleDestination},
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
		"plain http on a private address": {func(e *types.RegistryExecutor, _ []types.ResourceGroup) {
			e.Destination.Endpoint = "http://192.168.1.5:11434/v1"
		}, ""},
		// A local endpoint is local: no credential, a loopback or private IP.
		"remote https as local": {func(e *types.RegistryExecutor, _ []types.ResourceGroup) {
			e.Destination.Endpoint = "https://api.example.com/v1"
		}, RuleEndpoint},
		"a public IP as local": {func(e *types.RegistryExecutor, _ []types.ResourceGroup) {
			e.Destination.Endpoint = "https://203.0.113.7/v1"
		}, RuleEndpoint},
		"a name as local": {func(e *types.RegistryExecutor, _ []types.ResourceGroup) {
			e.Destination.Endpoint = "http://localhost:11434/v1"
		}, RuleEndpoint},
		"local with a credential": {func(e *types.RegistryExecutor, _ []types.ResourceGroup) {
			ref := types.Slug("api-key")
			e.CredentialRef = &ref
		}, RuleCredential},
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
	// What New does not admit in M1, built directly: Admits still checks the
	// routing label and refuses an unknown floor or trust class.
	e, _ := r.Executor(api)
	e.Destination.Verification = types.RegistryDestinationVerificationReported
	odd, _ := r.Executor(api)
	odd.ID, odd.Destination.TrustClass = local, "secret"
	attested, unlabelled := e, e
	attested.Destination.Verification, unlabelled.Destination.Verification = types.RegistryDestinationVerificationAttested, "rumoured"
	const att, unl = types.ExecutorID("exe_01m4tm99haemqt8es2qfbwj8pa"), types.ExecutorID("exe_01m4tm99haemr8ajyf09v4ytzb")
	r2 := Registry{executors: map[types.ExecutorID]types.RegistryExecutor{api: e, local: odd, att: attested, unl: unlabelled}}
	for name, c := range map[string]struct {
		dp   types.ProjectProfileDataPolicy
		id   types.ExecutorID
		want Rule
	}{
		"attested, verified asked": {types.ProjectProfileDataPolicy{Floor: "free_unknown", Routing: "verified", AllowedDestinations: both}, att, ""},
		"an unknown label":         {types.ProjectProfileDataPolicy{Floor: "free_unknown", Routing: "reported_ok", AllowedDestinations: both}, unl, RuleRouting},
		"reported, verified asked": {types.ProjectProfileDataPolicy{Floor: "free_unknown", Routing: "verified", AllowedDestinations: both}, api, RuleRouting},
		"reported, reported_ok":    {types.ProjectProfileDataPolicy{Floor: "free_unknown", Routing: "reported_ok", AllowedDestinations: both}, api, ""},
		"an unknown floor":         {types.ProjectProfileDataPolicy{Floor: "secret", Routing: "reported_ok", AllowedDestinations: both}, api, RuleFloor},
		"an unknown trust class":   {types.ProjectProfileDataPolicy{Floor: "free_unknown", Routing: "reported_ok", AllowedDestinations: both}, local, RuleFloor},
	} {
		err := r2.Admits(c.dp, c.id)
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
	fifo := filepath.Join(dir, "fifo.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"an empty FIFO": fifo,
		"a directory":   t.TempDir(),
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

// A FIFO whose writer holds a valid registry is refused unread: only a regular file
// is read, and nothing waits on a writer.
func TestLoadRefusesAFIFO(t *testing.T) {
	good, err := os.ReadFile(fixtures + "registry-stub.json")
	fifo := filepath.Join(t.TempDir(), "registry.json")
	if err == nil {
		err = syscall.Mkfifo(fifo, 0o600)
	}
	var w *os.File
	if err == nil {
		w, err = os.OpenFile(fifo, os.O_RDWR, 0) // a writer that never blocks
	}
	if err == nil {
		_, err = w.Write(good)
	}
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Load(fifo); done <- err }()
	select {
	case err := <-done:
		if refusedBy(err) != RuleUnreadable {
			t.Errorf("a FIFO: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Load read from a FIFO")
	}
	w.Close() // ends a blocked read
}

// The aliasing rule keys on the origin, so a reviewer on the builder's
// model stays refused under another destination id. New refuses the second
// id first (RuleDestination); distinct is checked on its own here.
func TestAliasedFamilyAcrossDestinationIDs(t *testing.T) {
	doc := clone(t, load(t, "registry-stub.json"))
	rev := &doc.Executors[1]
	rev.Family, rev.Model = "stub-other", "stub-builder-1"
	rev.Destination.ID, rev.Destination.Endpoint = "other", "http://127.0.0.1:18080/other"
	if got := refusedBy(Registry{}.distinct(doc.Executors)); got != RuleSameFamily {
		t.Fatalf("refused by %q, want %q", got, RuleSameFamily)
	}
	rev.Model = "stub-reviewer-1" // positive control: another model
	if err := (Registry{}).distinct(doc.Executors); err != nil {
		t.Fatal(err)
	}
}

// A registry cannot change once built (Astra, #246): nothing the caller
// does to the document it passed, or to an executor Executor returned,
// reaches it.
func TestRegistryIsImmutable(t *testing.T) {
	doc := load(t, "registry-stub.json")
	r, err := New(doc, groups(t))
	if err != nil {
		t.Fatal(err)
	}
	id := doc.Executors[0].ID
	*doc.Executors[0].CredentialRef = "changed-in-doc"
	got, _ := r.Executor(id)
	*got.CredentialRef = "changed-in-copy"
	got.Price.InputUsdMicrosPerMtok = 1
	if again, _ := r.Executor(id); *again.CredentialRef != "stub" || again.Price.InputUsdMicrosPerMtok != 1000000 {
		t.Fatalf("the registry changed under it: credential %s, price %d", *again.CredentialRef, again.Price.InputUsdMicrosPerMtok)
	}
}
