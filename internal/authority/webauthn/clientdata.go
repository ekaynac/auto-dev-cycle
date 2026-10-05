// SPDX-License-Identifier: Apache-2.0

package webauthn

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// clientData is the security fields of collected client data (§5.8.1).
type clientData struct {
	Type, Challenge, Origin string
	CrossOrigin             bool
}

// parseClientData reads clientDataJSON as one JSON object whose security
// fields carry their exact names and types, each at most once: type,
// challenge and origin strings, an optional crossOrigin boolean. topOrigin
// (present only for a cross-origin ceremony) is refused. Unknown fields are
// extensions and are skipped. Go's struct decoding is not used: it matches
// names case-insensitively, takes the last of repeated names and reads
// null as the zero value.
func parseClientData(raw []byte) (clientData, error) {
	var cd clientData
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return cd, errors.New("client data is not a JSON object")
	}
	strs := map[string]*string{"type": &cd.Type, "challenge": &cd.Challenge, "origin": &cd.Origin}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return cd, err
		}
		key, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return cd, err
		}
		_, str := strs[key]
		if !str && key != "crossOrigin" && key != "topOrigin" {
			continue // an extension field
		}
		if seen[key] {
			return cd, errors.New("client data repeats " + key)
		}
		seen[key] = true
		switch {
		case key == "topOrigin":
			return cd, errors.New("client data names a top origin")
		case str && (len(v) == 0 || v[0] != '"' || json.Unmarshal(v, strs[key]) != nil):
			return cd, errors.New("client data " + key + " is not a string")
		case !str && !bytes.Equal(v, []byte("true")) && !bytes.Equal(v, []byte("false")):
			return cd, errors.New("client data crossOrigin is not a boolean")
		case !str:
			cd.CrossOrigin = bytes.Equal(v, []byte("true"))
		}
	}
	if _, err := dec.Token(); err != nil {
		return cd, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return cd, errors.New("client data has trailing content")
	}
	for k := range strs {
		if !seen[k] {
			return cd, errors.New("client data has no " + k)
		}
	}
	return cd, nil
}
