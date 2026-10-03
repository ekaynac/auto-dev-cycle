// SPDX-License-Identifier: Apache-2.0

package schemagen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Node is a JSON value that keeps object key order, so generated structs
// follow the order of "properties" in the schema file.
type Node struct {
	Kind   Kind
	Keys   []string         // object keys in file order
	Fields map[string]*Node // object members
	Items  []*Node          // array elements
	Str    string           // string value, or the literal of a number
	Bool   bool
}

// Kind is the JSON type of a Node.
type Kind int

// JSON kinds.
const (
	Null Kind = iota
	Bool
	Number
	String
	Array
	Object
)

// Parse reads one JSON document, keeping object key order.
func Parse(data []byte) (*Node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after the document")
	}
	return n, nil
}

func parseValue(dec *json.Decoder) (*Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case nil:
		return &Node{Kind: Null}, nil
	case bool:
		return &Node{Kind: Bool, Bool: t}, nil
	case json.Number:
		return &Node{Kind: Number, Str: string(t)}, nil
	case string:
		return &Node{Kind: String, Str: t}, nil
	case json.Delim:
		if t == '[' {
			n := &Node{Kind: Array}
			for dec.More() {
				item, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				n.Items = append(n.Items, item)
			}
			_, err := dec.Token()
			return n, err
		}
		n := &Node{Kind: Object, Fields: map[string]*Node{}}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k := kt.(string)
			if _, dup := n.Fields[k]; dup {
				return nil, fmt.Errorf("duplicate key %q", k)
			}
			v, err := parseValue(dec)
			if err != nil {
				return nil, err
			}
			n.Keys = append(n.Keys, k)
			n.Fields[k] = v
		}
		_, err := dec.Token()
		return n, err
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

// Get returns the member k of an object node, or nil.
func (n *Node) Get(k string) *Node {
	if n == nil || n.Kind != Object {
		return nil
	}
	return n.Fields[k]
}

// Strings returns the string elements of an array node.
func (n *Node) Strings() []string {
	if n == nil {
		return nil
	}
	out := make([]string, 0, len(n.Items))
	for _, it := range n.Items {
		out = append(out, it.Str)
	}
	return out
}
