// SPDX-License-Identifier: Apache-2.0

// Package receipt is the only place that decides whether a receipt is
// valid, complete and applicable (docs/design/m0/06): the derivation of its
// outcome, reason, detail, verdict and conclusions digest; applicability to
// a requirement; reviewer independence; proposal binding; and the
// artifact-before-teardown check.
package receipt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/gowebpki/jcs"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/vocab"
)

// Blobs is the blob store as receipts need it: existence, and reading an
// instrument's payload schema.
type Blobs interface {
	Has(digest string) (bool, error)
	Open(digest string) (io.ReadCloser, error)
}

// ErrInvalid marks a receipt that is malformed against its instrument
// contract (another instrument, an undeclared artifact, a payload without a
// schema or failing it). An invalid receipt is refused, never derived.
var ErrInvalid = errors.New("receipt: invalid")

// Derived is what the controller computes for a receipt; producers never
// supply it.
type Derived struct {
	Outcome           vocab.Outcome
	Reason            vocab.Reason
	Detail            string
	Verdict           *types.ReceiptVerdictValue // review receipts only
	ConclusionsDigest types.Digest
}

// Derive computes a receipt's outcome, reason, detail, verdict and
// conclusions digest under its instrument contract (06 "Derivation"):
// completeness first, then aggregation.
func Derive(c types.InstrumentContract, r types.Receipt, blobs Blobs) (Derived, error) {
	if r.Instrument.Name != c.Name || r.Instrument.Version != c.Version || r.Instrument.Digest != c.Digest {
		return Derived{}, fmt.Errorf("%w: produced by %s %s (%s), the contract is %s %s (%s)", ErrInvalid,
			r.Instrument.Name, r.Instrument.Version, r.Instrument.Digest, c.Name, c.Version, c.Digest)
	}
	if got := subjectType(r.Subject); got != string(c.SubjectType) {
		return Derived{}, fmt.Errorf("%w: the receipt is about a %s, the instrument's subject type is %s", ErrInvalid, got, c.SubjectType)
	}
	if err := checkPredicates(c, r, blobs); err != nil {
		return Derived{}, err
	}
	conclusions, err := conclusionsDigest(r)
	if err != nil {
		return Derived{}, err
	}
	d := Derived{ConclusionsDigest: conclusions}
	if missing, err := incomplete(c, r, blobs); err != nil {
		return Derived{}, err
	} else if missing != "" {
		d.Outcome, d.Reason, d.Detail = vocab.CouldNotRun, vocab.ReasonReceiptIncomplete, missing
	} else {
		d.Outcome, d.Reason, d.Detail = aggregate(r.Predicates)
	}
	if r.Kind == types.ReceiptKindReview {
		v := types.ReceiptVerdictValueNoGo
		if d.Outcome == vocab.Empty {
			v = types.ReceiptVerdictValueGo
		}
		d.Verdict = &v
	}
	return d, nil
}

// Check refuses a receipt whose recorded derived fields differ from the
// derivation: they are never accepted from the producer.
func Check(c types.InstrumentContract, r types.Receipt, blobs Blobs) error {
	d, err := Derive(c, r, blobs)
	if err != nil {
		return err
	}
	switch {
	case vocab.Outcome(r.Outcome) != d.Outcome || r.Reason != d.Reason || r.Detail != d.Detail:
		return fmt.Errorf("%w: recorded %s/%s, derived %s/%s", ErrInvalid, r.Outcome, r.Reason.String(), d.Outcome, d.Reason.String())
	case (r.Verdict == nil) != (d.Verdict == nil) || (r.Verdict != nil && *r.Verdict != *d.Verdict):
		return fmt.Errorf("%w: the verdict is not the derived one", ErrInvalid)
	case r.ConclusionsDigest != d.ConclusionsDigest:
		return fmt.Errorf("%w: the conclusions digest is not the derived one", ErrInvalid)
	}
	return nil
}

// checkPredicates: each predicate is named once, declared by the contract,
// cites only declared artifacts, and carries a payload only under a payload
// schema, which it then satisfies (rule 7).
func checkPredicates(c types.InstrumentContract, r types.Receipt, blobs Blobs) error {
	declared := map[string]bool{}
	for _, p := range c.RequiredPredicates {
		declared[p.Name] = true
	}
	for _, n := range c.OptionalPredicates {
		declared[n] = true
	}
	var schema *jsonschema.Schema
	seen := map[string]bool{}
	for _, p := range r.Predicates {
		switch {
		case seen[p.Name]:
			return fmt.Errorf("%w: predicate %s appears twice", ErrInvalid, p.Name)
		case !declared[p.Name]:
			return fmt.Errorf("%w: predicate %s is not in the instrument contract", ErrInvalid, p.Name)
		}
		seen[p.Name] = true
		for _, e := range p.Evidence {
			if _, ok := c.ArtifactClasses[e.Artifact]; !ok {
				return fmt.Errorf("%w: predicate %s cites artifact %s, which the instrument does not declare", ErrInvalid, p.Name, e.Artifact)
			}
		}
		if len(p.Payload) == 0 {
			continue
		}
		if c.PayloadSchema == nil {
			return fmt.Errorf("%w: predicate %s carries a payload, but the instrument declares no payload schema", ErrInvalid, p.Name)
		}
		if schema == nil {
			var err error
			if schema, err = loadSchema(string(*c.PayloadSchema), blobs); err != nil {
				return err
			}
		}
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(p.Payload))
		if err != nil {
			return fmt.Errorf("%w: predicate %s payload: %v", ErrInvalid, p.Name, err)
		}
		if err := schema.Validate(v); err != nil {
			return fmt.Errorf("%w: predicate %s payload: %v", ErrInvalid, p.Name, err)
		}
	}
	return nil
}

// loadSchema compiles an instrument's payload schema from the blob store.
func loadSchema(digest string, blobs Blobs) (*jsonschema.Schema, error) {
	rc, err := blobs.Open(digest)
	if err != nil {
		return nil, fmt.Errorf("receipt: payload schema %s: %w", digest, err)
	}
	defer rc.Close()
	doc, err := jsonschema.UnmarshalJSON(rc)
	if err != nil {
		return nil, fmt.Errorf("receipt: payload schema %s: %w", digest, err)
	}
	comp := jsonschema.NewCompiler()
	// A payload schema is self-contained: no $ref may load anything (no file,
	// no network), so a schema can never read the controller's files.
	comp.UseLoader(refuseLoader{})
	url := "urn:adc:payload-schema:" + digest
	if err := comp.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("receipt: payload schema %s: %w", digest, err)
	}
	s, err := comp.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("receipt: payload schema %s: %w", digest, err)
	}
	return s, nil
}

// incomplete names what a complete receipt lacks: a required predicate, a
// required evidence media type, or an evidence blob that is not published.
// An empty or partial predicate list can never derive empty.
func incomplete(c types.InstrumentContract, r types.Receipt, blobs Blobs) (string, error) {
	byName := map[string]types.Predicate{}
	for _, p := range r.Predicates {
		byName[p.Name] = p
	}
	var missing []string
	for _, req := range c.RequiredPredicates {
		p, ok := byName[req.Name]
		if !ok {
			missing = append(missing, "predicate "+req.Name)
			continue
		}
		media := map[string]bool{}
		for _, e := range p.Evidence {
			media[e.MediaType] = true
		}
		for _, m := range req.EvidenceRequired {
			if !media[m] {
				missing = append(missing, fmt.Sprintf("%s evidence %s", req.Name, m))
			}
		}
	}
	for _, p := range r.Predicates {
		for _, e := range p.Evidence {
			ok, err := blobs.Has(e.Blob)
			if err != nil {
				return "", err
			}
			if !ok {
				missing = append(missing, fmt.Sprintf("%s evidence blob %s", p.Name, e.Blob))
			}
		}
	}
	if len(missing) == 0 {
		return "", nil
	}
	return "missing " + strings.Join(missing, "; "), nil
}

// aggregate: any failed → failed; else any could_not_run → could_not_run;
// else any found → found; else empty. The receipt takes the reason and
// detail of the first predicate with the deciding outcome; an empty receipt
// is completed_clean.
func aggregate(ps []types.Predicate) (vocab.Outcome, vocab.Reason, string) {
	for _, o := range []vocab.Outcome{vocab.Failed, vocab.CouldNotRun, vocab.Found} {
		for _, p := range ps {
			if vocab.Outcome(p.Outcome) == o {
				return o, p.Reason, p.Name + ": " + p.Detail
			}
		}
	}
	return vocab.Empty, vocab.ReasonCompletedClean, ""
}

// conclusionsDigest is the digest of a receipt's typed conclusions (rule
// 2a): the demand key and each predicate's name, outcome and reason, in
// name order. (Per-step outcomes and restrictions join it when the
// envelope carries them.)
func conclusionsDigest(r types.Receipt) (types.Digest, error) {
	type conclusion struct {
		Name    string `json:"name"`
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	}
	cs := make([]conclusion, 0, len(r.Predicates))
	for _, p := range r.Predicates {
		cs = append(cs, conclusion{p.Name, string(p.Outcome), p.Reason.String()})
	}
	slices.SortFunc(cs, func(a, b conclusion) int { return strings.Compare(a.Name, b.Name) })
	var key *string
	if d := demandOf(r.Subject); d != nil {
		k := string(d.Key)
		key = &k
	}
	b, err := json.Marshal(struct {
		Demand     *string      `json:"demand"`
		Predicates []conclusion `json:"predicates"`
	}{key, cs})
	if err != nil {
		return "", err
	}
	c, err := jcs.Transform(b)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return types.Digest("sha256:" + hex.EncodeToString(sum[:])), nil
}

// refuseLoader refuses every external schema reference.
type refuseLoader struct{}

func (refuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("receipt: a payload schema may not reference %s", url)
}

// subjectType is the type of a typed subject.
func subjectType(s types.Subject) string {
	switch s.Variant.(type) {
	case *types.TreeSubject:
		return "tree"
	case *types.ProposalSubject:
		return "proposal"
	case *types.ObjectRevisionSubject:
		return "object_revision"
	case *types.EnvironmentSubject:
		return "environment"
	case *types.CallSubject:
		return "call"
	}
	return "none"
}

// demandOf is the subject's evidence demand, if any.
func demandOf(s types.Subject) *types.DemandValue {
	switch v := s.Variant.(type) {
	case *types.TreeSubject:
		return v.Demand
	case *types.ProposalSubject:
		return v.Demand
	case *types.ObjectRevisionSubject:
		return v.Demand
	case *types.EnvironmentSubject:
		return v.Demand
	case *types.CallSubject:
		return v.Demand
	}
	return nil
}
