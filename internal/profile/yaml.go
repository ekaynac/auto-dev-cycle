// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"go.yaml.in/yaml/v3"
)

// MaxBytes bounds a profile document before it is parsed. A real profile is
// a few kilobytes; the bound keeps the parse trees of a hostile document
// small.
const MaxBytes = 64 << 10

// jsonInt is JSON's integer grammar. YAML integers are passed on exactly as
// written, never through float64, so the schema sees what the author wrote.
var jsonInt = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// toJSON returns the profile as JSON. A JSON document (it starts with '{') is
// passed on unchanged to the contract's strict decoding (which refuses
// duplicate names, invalid UTF-8 and non-integral numbers); anything else is
// read as exactly one YAML document.
func toJSON(raw []byte) ([]byte, error) {
	if len(raw) > MaxBytes {
		return nil, fmt.Errorf("a profile is at most %d bytes (this one is %d)", MaxBytes, len(raw))
	}
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '{' {
		return raw, nil
	}
	return yamlToJSON(raw)
}

// yamlToJSON converts exactly one YAML document to JSON. It refuses what
// JSON cannot say or what would make the document mean more than it shows:
// anchors and aliases, merge keys, non-string or duplicate keys, explicit
// tags other than the node's own core type, and fractional numbers (a
// profile has none).
func yamlToJSON(raw []byte) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("a profile holds exactly one YAML document")
	}
	v, err := plain(&doc)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func plain(n *yaml.Node) (any, error) {
	if n.Anchor != "" {
		return nil, fmt.Errorf("line %d: YAML anchors are not allowed in a profile", n.Line)
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return nil, errors.New("an empty profile")
		}
		return plain(n.Content[0])
	case yaml.AliasNode:
		return nil, fmt.Errorf("line %d: YAML aliases are not allowed in a profile", n.Line)
	case yaml.MappingNode:
		if n.ShortTag() != "!!map" {
			return nil, fmt.Errorf("line %d: the YAML tag %s is not allowed on a mapping", n.Line, n.ShortTag())
		}
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" {
				return nil, fmt.Errorf("line %d: keys are strings (a merge key or a %s key is not allowed)", k.Line, k.ShortTag())
			}
			if k.Anchor != "" {
				return nil, fmt.Errorf("line %d: YAML anchors are not allowed in a profile", k.Line)
			}
			if _, dup := m[k.Value]; dup {
				return nil, fmt.Errorf("line %d: key %q is defined twice", k.Line, k.Value)
			}
			v, err := plain(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			m[k.Value] = v
		}
		return m, nil
	case yaml.SequenceNode:
		if n.ShortTag() != "!!seq" {
			return nil, fmt.Errorf("line %d: the YAML tag %s is not allowed on a sequence", n.Line, n.ShortTag())
		}
		s := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := plain(c)
			if err != nil {
				return nil, err
			}
			s = append(s, v)
		}
		return s, nil
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!str", "!!null", "!!bool":
			var v any
			if err := n.Decode(&v); err != nil {
				return nil, fmt.Errorf("line %d: %w", n.Line, err)
			}
			return v, nil
		case "!!int":
			if !jsonInt.MatchString(n.Value) {
				return nil, fmt.Errorf("line %d: write the integer %q in plain decimal", n.Line, n.Value)
			}
			return json.Number(n.Value), nil
		case "!!float":
			return nil, fmt.Errorf("line %d: %q: a profile has no fractional numbers (quote it if it is text)", n.Line, n.Value)
		}
		return nil, fmt.Errorf("line %d: the YAML tag %s is not allowed in a profile; quote the value", n.Line, n.ShortTag())
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
}
