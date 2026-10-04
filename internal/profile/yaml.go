// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"
)

// yamlToJSON converts exactly one YAML document (JSON is YAML) to JSON. It
// refuses what JSON cannot say or what would make the document mean more
// than it shows: anchors and aliases, merge keys, non-string keys, and
// tags other than the core scalar types, and duplicate keys (decoding into
// a yaml.Node does not refuse them by itself).
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
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" {
				return nil, fmt.Errorf("line %d: keys are strings (a merge key or a %s key is not allowed)", k.Line, k.ShortTag())
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
		case "!!str", "!!null", "!!bool", "!!int", "!!float":
			var v any
			if err := n.Decode(&v); err != nil {
				return nil, fmt.Errorf("line %d: %w", n.Line, err)
			}
			return v, nil
		}
		return nil, fmt.Errorf("line %d: the YAML tag %s is not allowed in a profile; quote the value", n.Line, n.ShortTag())
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
}
