// SPDX-License-Identifier: Apache-2.0

// Package ids mints and checks controller identifiers: standard TypeIDs with
// a UUIDv7 suffix (docs/design/m0/02 "Identifiers", Q2). Only the controller
// mints ids, and only with a registered prefix. The registry is the set of
// entity id definitions in schemas/common/defs.json, so the schema and the
// minting code cannot disagree about which prefixes exist.
package ids

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"sync"

	"go.jetify.com/typeid"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/schemas"
)

// typeIDRef is the only way an entity id definition may refer to TypeID.
const typeIDRef = "#/$defs/TypeID"

var (
	// entityPattern is how defs.json refines TypeID for one entity: "^prj_".
	entityPattern = regexp.MustCompile(`^\^([a-z]{2,3})_$`)
	// prefixLike is any pattern of the prefix form, valid length or not.
	prefixLike = regexp.MustCompile(`^\^[a-z]+_$`)
)

var (
	defsFS   = schemas.FS      // replaced in tests
	loadOnce = new(sync.Once)  // replaced in tests
	registry map[string]string // prefix → definition name, e.g. "prj" → "ProjectID"
	loadErr  error
)

func load() (map[string]string, error) {
	loadOnce.Do(func() { registry, loadErr = parse(defsFS()) })
	return registry, loadErr
}

// parse collects the entity id definitions of common/defs.json: every entry
// that is exactly {"$ref": "#/$defs/TypeID", "pattern": "^<prefix>_"}. An
// entry that mentions TypeID or carries a prefix pattern in any other form is
// an error, so a slip in a definition can never silently drop a prefix.
func parse(fsys fs.FS) (map[string]string, error) {
	b, err := fs.ReadFile(fsys, "common/defs.json")
	if err != nil {
		return nil, err
	}
	// The contract runtime refuses duplicate keys, which encoding/json would
	// settle silently (the last one wins).
	if err := contract.Validate("common/defs", b); err != nil {
		return nil, fmt.Errorf("ids: common/defs.json: %w", err)
	}
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("ids: common/defs.json: %w", err)
	}
	out := map[string]string{}
	for name, raw := range doc.Defs {
		if name == "TypeID" || !bytes.Contains(raw, []byte("TypeID")) && !hasPrefixPattern(raw) {
			continue
		}
		var d struct {
			Ref     string `json:"$ref"`
			Pattern string `json:"pattern"`
		}
		if err := contract.UnmarshalStrict(raw, &d); err != nil {
			return nil, fmt.Errorf("ids: %s must be exactly {$ref, pattern}: %w", name, err)
		}
		m := entityPattern.FindStringSubmatch(d.Pattern)
		if d.Ref != typeIDRef || m == nil {
			return nil, fmt.Errorf("ids: %s must be {\"$ref\": %q, \"pattern\": \"^<prefix>_\"}, got $ref %q, pattern %q",
				name, typeIDRef, d.Ref, d.Pattern)
		}
		if prev, dup := out[m[1]]; dup {
			return nil, fmt.Errorf("ids: prefix %q is defined by both %s and %s", m[1], prev, name)
		}
		out[m[1]] = name
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ids: common/defs.json defines no entity ids")
	}
	return out, nil
}

// hasPrefixPattern: the entry carries a pattern of the entity-prefix form.
func hasPrefixPattern(raw json.RawMessage) bool {
	var d struct {
		Pattern string `json:"pattern"`
	}
	return json.Unmarshal(raw, &d) == nil && prefixLike.MatchString(d.Pattern)
}

// Prefixes lists the registered prefixes, sorted.
func Prefixes() ([]string, error) {
	r, err := load()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(r))
	for p := range r {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

func registered(prefix string) error {
	r, err := load()
	if err != nil {
		return err
	}
	if _, ok := r[prefix]; !ok {
		return fmt.Errorf("ids: unregistered prefix %q", prefix)
	}
	return nil
}

// New mints a fresh id with a registered prefix.
func New(prefix string) (string, error) {
	if err := registered(prefix); err != nil {
		return "", err
	}
	id, err := typeid.WithPrefix(prefix)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// Check parses s and requires the given registered prefix and the schema's
// adc-typeid suffix (UUIDv7, RFC 9562 variant), checked by the function the
// schema itself uses, so the two cannot disagree.
func Check(s, prefix string) error {
	if err := registered(prefix); err != nil {
		return err
	}
	id, err := typeid.FromString(s)
	if err != nil {
		return fmt.Errorf("ids: %q is not a TypeID: %w", s, err)
	}
	if id.Prefix() != prefix {
		return fmt.Errorf("ids: %q has prefix %q, want %q", s, id.Prefix(), prefix)
	}
	if err := contract.CheckTypeID(s); err != nil {
		return fmt.Errorf("ids: %q: %w", s, err)
	}
	return nil
}
