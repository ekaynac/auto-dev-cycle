// SPDX-License-Identifier: Apache-2.0

// Package registry is the controller's executor registry and M1's route and
// accounting envelope (docs/design/m1/00-slice-plan.md P7, Q2, Q8).
//
// Decisions:
//   - The registry is checked once, at start, against the resource groups;
//     whatever M1 does not implement is refused, never approximated. The
//     schema's enums stay open (a governed upstream, the attested tier,
//     D48), so a later milestone widens this check, not the schema.
//   - M1's routes: the http-chat adapter, to a direct API (one
//     account_quota group in tokens, known or estimated) or a local endpoint
//     (one concurrency group in slots, known), every group on_settle, the
//     routing label verified. Brokers are refused.
//   - Every reviewer's family differs from every builder's, and no family is
//     unknown (06 rule 3): the difference is a registry property.
//   - The registry enforces and never selects (D48): Admits checks a
//     placement the scheduler chose; nothing here picks an executor.
//   - Kind, family and cost basis are the operator's declarations: the
//     registry checks them for consistency and the gateway enforces them,
//     but neither establishes them.
//   - A destination is identified by its origin, not its free id: one
//     origin has one id, and the account, group and aliasing rules key on
//     the origin, so relabelling an endpoint evades none of them.
package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"syscall"
	"unicode"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/mutation"
	"github.com/ekaynac/auto-dev-cycle/internal/receipt"
)

// Schema is the registry document's schema.
const Schema = "config/executor-registry"

// Adapter is the one harness kind M1 runs (P13).
const Adapter = "http-chat"

const maxFile = 1 << 20 // bytes

// Rule is a refusal's fixed text: it never holds a value read from the file.
type Rule string

// The rules New and Admits refuse by.
const (
	RuleUnreadable    Rule = "the registry cannot be opened or read, is larger than 1 MiB, or does not satisfy " + Schema
	RuleDuplicate     Rule = "an executor id is registered twice"
	RuleRoles         Rule = "the registry needs at least one builder and one reviewer"
	RuleUnknownFamily Rule = "the family is unknown, which is never independent"
	RuleSameFamily    Rule = "a reviewer's family, or its endpoint origin and model, equals a builder's"
	RuleAdapter       Rule = "the adapter is not http-chat, the one M1 runs"
	RuleKind          Rule = "the destination kind is not direct_api or local_endpoint (brokers are refused; a governed upstream is M2 or later)"
	RuleVerification  Rule = "the routing label is not verified (attested is M2)"
	RuleTrustClass    Rule = "only a local endpoint's trust class can be own"
	RuleDestination   Rule = "one destination id is registered with different properties, or one endpoint origin under two ids"
	RuleEndpoint      Rule = "the endpoint is not an http(s) URL without credential, query, fragment, whitespace or backslash; a direct API's is https unless on a loopback address, a local endpoint's host a loopback, RFC 1918 or fc00::/7 IP literal"
	RuleCredential    Rule = "a direct API needs a credential reference, and a local endpoint has none"
	RuleCredentialRef Rule = "one credential reference is registered at two endpoint origins"
	RuleGroup         Rule = "the group is not configured, does not list the executor, another group lists it, lists an unregistered executor or another destination, or one account or endpoint is split across groups"
	RuleGroupKind     Rule = "the group is not a direct API's account_quota in tokens with a known or estimated limit, or a local endpoint's concurrency in slots with a known limit, released on_settle"
	RulePriceBasis    Rule = "a price is given exactly for cash and subscription_equivalent; a local endpoint is local, a direct API is not"
	RuleNotRegistered Rule = "the executor is not registered"
	RuleFloor         Rule = "the destination's trust class is below the project's data floor, or either is unknown"
	RuleRouting       Rule = "the project requires verified routing"
	RuleNotAllowed    Rule = "the destination is not among the project's allowed destinations"
	RuleUsageProfile  Rule = "an empty allowed_destinations derives from a usage profile, which M1 does not have"
)

// Refusal is New's or Admits' refusal: the executor's index in the
// registry (-1 for the registry as a whole) and the rule.
type Refusal struct {
	Executor int
	Rule     Rule
}

func (r *Refusal) Error() string {
	if r.Executor < 0 {
		return "registry: " + string(r.Rule)
	}
	return fmt.Sprintf("registry: executor %d: %s", r.Executor, r.Rule)
}

// Registry is the checked registry. It cannot change once built.
type Registry struct {
	executors map[types.ExecutorID]types.RegistryExecutor
	limits    map[types.ResourceGroupID]int64 // each group's limit, where its capacity model states one
}

// Load reads the registry file, a regular file and never a link, and checks it
// against its schema as written, so no field is dropped by decoding.
func Load(path string) (types.ExecutorRegistry, error) {
	refused := &Refusal{Executor: -1, Rule: RuleUnreadable}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return types.ExecutorRegistry{}, refused
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return types.ExecutorRegistry{}, refused
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil || len(raw) > maxFile {
		return types.ExecutorRegistry{}, refused
	}
	var doc types.ExecutorRegistry
	if contract.Validate(Schema, raw) != nil || json.Unmarshal(raw, &doc) != nil {
		return types.ExecutorRegistry{}, refused
	}
	return doc, nil
}

// New checks the registry against M1's envelope and the resource groups
// (which capacity.NewReservations checks on their own) and returns it.
func New(doc types.ExecutorRegistry, groups []types.ResourceGroup) (Registry, error) {
	if contract.Validate(Schema, mustJSON(doc)) != nil {
		return Registry{}, &Refusal{Executor: -1, Rule: RuleUnreadable}
	}
	r := Registry{executors: map[types.ExecutorID]types.RegistryExecutor{}, limits: map[types.ResourceGroupID]int64{}}
	for _, g := range groups {
		if l := g.CapacityModel.Limit; l != nil {
			r.limits[g.ID] = *l
		}
	}
	destinations := map[types.Slug]types.RegistryDestination{}
	ids := map[string]types.Slug{} // origin → destination id
	for i, e := range doc.Executors {
		rule := check(e, groups)
		if d, seen := destinations[e.Destination.ID]; rule == "" && seen && string(mustJSON(d)) != string(mustJSON(e.Destination)) {
			rule = RuleDestination
		}
		if id, seen := ids[origin(e)]; rule == "" && seen && id != e.Destination.ID {
			rule = RuleDestination
		}
		if _, dup := r.executors[e.ID]; rule == "" && dup {
			rule = RuleDuplicate
		}
		if rule != "" {
			return Registry{}, &Refusal{Executor: i, Rule: rule}
		}
		destinations[e.Destination.ID], ids[origin(e)] = e.Destination, e.Destination.ID
		r.executors[e.ID] = copyOf(e) // the caller keeps doc: nothing it changes reaches r
	}
	if err := r.accounts(doc.Executors, groups); err != nil {
		return Registry{}, err
	}
	return r, r.distinct(doc.Executors)
}

// origin is the destination's normalised origin, scheme://host:port in
// lower case with the scheme's default port made explicit, and an IP
// literal in its one canonical form (IPv4-mapped IPv6 unmapped), so
// [::1] and [0:0::1] are one origin. check has already parsed the
// endpoint.
func origin(e types.RegistryExecutor) string {
	u, err := url.Parse(e.Destination.Endpoint)
	if err != nil {
		return "\x00" + e.Destination.Endpoint // never reached after check
	}
	scheme, port, host := strings.ToLower(u.Scheme), u.Port(), strings.ToLower(u.Hostname())
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// accounts ties groups to what they limit (Q8): executors on one account
// (a direct API's origin and credential) or one local endpoint (its
// origin) draw from one group, a group
// limits one destination, and every member of a group the registry names
// is registered, so nothing unseen draws from it. A credential reference
// names one origin's credential: the gateway injects it there only (P11).
func (r Registry) accounts(all []types.RegistryExecutor, groups []types.ResourceGroup) error {
	account := map[string]types.ResourceGroupID{}
	limits := map[types.ResourceGroupID]types.Slug{}
	refs := map[types.Slug]string{} // credential reference → origin
	for i, e := range all {
		key := origin(e) // a local endpoint has no credential
		if e.CredentialRef != nil {
			if o, seen := refs[*e.CredentialRef]; seen && o != origin(e) {
				return &Refusal{Executor: i, Rule: RuleCredentialRef}
			}
			refs[*e.CredentialRef] = origin(e)
			key += "\x00" + string(*e.CredentialRef)
		}
		g, seen := account[key]
		d, named := limits[e.Group]
		if (seen && g != e.Group) || (named && d != e.Destination.ID) {
			return &Refusal{Executor: i, Rule: RuleGroup}
		}
		account[key], limits[e.Group] = e.Group, e.Destination.ID
	}
	for _, g := range groups {
		for _, m := range g.Members {
			if _, ok := r.executors[m]; !ok && limits[g.ID] != "" {
				return &Refusal{Executor: -1, Rule: RuleGroup}
			}
		}
	}
	return nil
}

// distinct refuses a registry with no builder or no reviewer, or whose
// reviewer shares a builder's family.
func (r Registry) distinct(all []types.RegistryExecutor) error {
	builders := map[types.Name]bool{}
	served := map[string]bool{} // a builder's origin and model: one model under two family names
	route := func(e types.RegistryExecutor) string { return origin(e) + "\x00" + e.Model }
	for _, e := range all {
		builder := e.Role == types.RegistryExecutorRoleBuilder
		builders[e.Family], served[route(e)] = builders[e.Family] || builder, served[route(e)] || builder
	}
	reviewers := 0
	for i, e := range all {
		if e.Role != types.RegistryExecutorRoleReviewer {
			continue
		}
		if reviewers++; builders[e.Family] || served[route(e)] {
			return &Refusal{Executor: i, Rule: RuleSameFamily}
		}
	}
	if reviewers == 0 || reviewers == len(all) {
		return &Refusal{Executor: -1, Rule: RuleRoles}
	}
	return nil
}

// check is one executor's envelope: family, adapter, destination, group
// and price basis.
func check(e types.RegistryExecutor, groups []types.ResourceGroup) Rule {
	direct := e.Destination.Kind == types.RegistryDestinationKindDirectApi
	switch {
	case e.Family == receipt.UnknownFamily:
		return RuleUnknownFamily
	case e.Adapter != Adapter:
		return RuleAdapter
	case !direct && e.Destination.Kind != types.RegistryDestinationKindLocalEndpoint:
		return RuleKind
	case e.Destination.Verification != types.RegistryDestinationVerificationVerified:
		return RuleVerification
	case direct && e.Destination.TrustClass == types.RegistryDestinationTrustClassOwn:
		return RuleTrustClass
	case !endpointOK(e.Destination.Endpoint, direct):
		return RuleEndpoint
	case direct == (e.CredentialRef == nil):
		return RuleCredential
	}
	priced := e.CostBasis == types.RegistryExecutorCostBasisCash || e.CostBasis == types.RegistryExecutorCostBasisSubscriptionEquivalent
	if priced != (e.Price != nil) || direct == (e.CostBasis == types.RegistryExecutorCostBasisLocal) {
		return RulePriceBasis
	}
	return checkGroup(e, direct, groups)
}

// checkGroup: the executor draws from exactly the group it names, of its
// route's kind (Q8).
func checkGroup(e types.RegistryExecutor, direct bool, groups []types.ResourceGroup) Rule {
	var own *types.ResourceGroup
	for i, g := range groups {
		if slices.Contains(g.Members, e.ID) != (g.ID == e.Group) {
			return RuleGroup
		}
		if g.ID == e.Group {
			own = &groups[i]
		}
	}
	if own == nil {
		return RuleGroup
	}
	m := own.CapacityModel
	quota := own.Kind == types.ResourceGroupKindAccountQuota && m.Unit == types.CapacityUnitTokens &&
		m.QuotaState != types.ResourceGroupCapacityModelQuotaStateUnknown
	slots := own.Kind == types.ResourceGroupKindConcurrency && m.Unit == types.CapacityUnitSlots &&
		m.QuotaState == types.ResourceGroupCapacityModelQuotaStateKnown
	if own.ReleaseRule != types.ResourceGroupReleaseRuleOnSettle || (direct && !quota) || (!direct && !slots) {
		return RuleGroupKind
	}
	return ""
}

// endpointOK: an absolute http(s) URL with a host and no userinfo, query,
// fragment, whitespace or backslash. A direct API's is https, or plain
// http to a loopback IP literal, whose traffic stays on the host. A local
// endpoint's host is a loopback, RFC 1918 or fc00::/7 IP literal (what
// netip's IsPrivate admits): a name could
// resolve elsewhere, and a public address is not local.
func endpointOK(s string, direct bool) bool {
	u, err := url.Parse(s)
	if err != nil || strings.ContainsFunc(s, func(c rune) bool { return c == '\\' || unicode.IsSpace(c) }) ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.Hostname() == "" ||
		(u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if !direct {
		return err == nil && (ip.IsLoopback() || ip.IsPrivate())
	}
	return u.Scheme == "https" || (err == nil && ip.IsLoopback())
}

// Family is the executor's model family, receipt.UnknownFamily for one the
// registry does not hold (graph.Families).
func (r Registry) Family(id types.ExecutorID) string {
	if e, ok := r.executors[id]; ok {
		return string(e.Family)
	}
	if mutation.Is(mutation.UnknownFamilyIndependent) {
		return string(id) // a family of its own: an unknown executor counted independent
	}
	return receipt.UnknownFamily
}

// Executor is the registered executor, a deep copy: a caller changing it
// (its credential reference, price or retention) changes nothing here.
func (r Registry) Executor(id types.ExecutorID) (types.RegistryExecutor, bool) {
	e, ok := r.executors[id]
	if !ok {
		return types.RegistryExecutor{}, false
	}
	return copyOf(e), true
}

// Limit is group's limit, as the groups the registry was built over state
// it; false when none does.
func (r Registry) Limit(group types.ResourceGroupID) (int64, bool) {
	l, ok := r.limits[group]
	return l, ok
}

// Executors are the registered executors of role, in id order.
func (r Registry) Executors(role types.RegistryExecutorRole) []types.ExecutorID {
	var out []types.ExecutorID
	for id, e := range r.executors {
		if e.Role == role {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// copyOf deep-copies an executor through its JSON, so no pointer is shared.
func copyOf(e types.RegistryExecutor) types.RegistryExecutor {
	var c types.RegistryExecutor
	if err := json.Unmarshal(mustJSON(e), &c); err != nil {
		panic(err) // a generated type round-trips
	}
	return c
}

// floors orders trust classes and data floors: a destination meets a floor
// at or above its own rank.
var floors = map[string]int{"own": 0, "contracted": 1, "free_unknown": 2}

// labels orders routing labels, and routings what a project asks for:
// attested is cumulative over verified (01 §2.2).
var (
	labels   = map[string]int{"reported": 0, "verified": 1, "attested": 2}
	routings = map[string]int{"reported_ok": 0, "verified": 1}
)

// Admits checks that a project's data policy admits the executor's
// destination: its trust class meets the floor, its routing label the
// project's routing, and the project allows it by name. An empty
// allowed_destinations derives from a usage profile (07), which M1 has
// not, so it is refused rather than read as "any".
func (r Registry) Admits(dp types.ProjectProfileDataPolicy, id types.ExecutorID) error {
	e, ok := r.executors[id]
	has, hok := floors[string(e.Destination.TrustClass)]
	needs, nok := floors[string(dp.Floor)]
	label, lok := labels[string(e.Destination.Verification)]
	asked, aok := routings[string(dp.Routing)]
	rule := Rule("")
	switch {
	case !ok:
		rule = RuleNotRegistered
	case !hok || !nok || has > needs:
		rule = RuleFloor
	case !lok || !aok || label < asked:
		rule = RuleRouting
	case len(dp.AllowedDestinations) == 0:
		rule = RuleUsageProfile
	case !slices.Contains(dp.AllowedDestinations, e.Destination.ID):
		rule = RuleNotAllowed
	default:
		return nil
	}
	return &Refusal{Executor: -1, Rule: rule}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // a generated type always marshals
	}
	return b
}
