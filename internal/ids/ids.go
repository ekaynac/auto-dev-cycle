// SPDX-License-Identifier: Apache-2.0

// Package ids mints and checks controller identifiers: standard TypeIDs with
// a UUIDv7 suffix (docs/design/m0/02 "Identifiers", Q2). Only the controller
// mints ids, and only with a registered prefix. The registry is the set of
// entity id definitions in schemas/common/defs.json, so the schema and the
// minting code cannot disagree about which prefixes exist.
package ids

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"sync"

	"go.jetify.com/typeid"

	"github.com/ekaynac/auto-dev-cycle/schemas"
)

// entityPattern is how defs.json refines TypeID for one entity: "^prj_".
var entityPattern = regexp.MustCompile(`^\^([a-z]{2,3})_$`)

var (
	loadOnce sync.Once
	registry map[string]string // prefix → definition name, e.g. "prj" → "ProjectID"
	loadErr  error
)

func load() (map[string]string, error) {
	loadOnce.Do(func() { registry, loadErr = parse(schemas.FS()) })
	return registry, loadErr
}

// parse collects every $defs entry of common/defs.json that refines TypeID
// with a prefix pattern.
func parse(fsys fs.FS) (map[string]string, error) {
	b, err := fs.ReadFile(fsys, "common/defs.json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Defs map[string]struct {
			Ref     string `json:"$ref"`
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("ids: common/defs.json: %w", err)
	}
	out := map[string]string{}
	for name, d := range doc.Defs {
		if d.Ref != "#/$defs/TypeID" {
			continue
		}
		m := entityPattern.FindStringSubmatch(d.Pattern)
		if m == nil {
			return nil, fmt.Errorf("ids: %s refines TypeID with %q, want a ^<prefix>_ pattern", name, d.Pattern)
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

// Check parses s and requires the given registered prefix and a UUIDv7
// suffix (RFC 9562 version 7, variant 10), as the schema's adc-typeid does.
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
	u := id.UUIDBytes()
	if u[6]>>4 != 7 || u[8]>>6 != 2 {
		return fmt.Errorf("ids: %q is not a UUIDv7 TypeID", s)
	}
	return nil
}
