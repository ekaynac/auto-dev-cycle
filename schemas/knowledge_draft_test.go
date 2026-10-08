// SPDX-License-Identifier: Apache-2.0

package schemas_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// draftDocs returns one valid document per draft knowledge record
// (docs/design/m0/10 "Shared knowledge spec draft", 15 D5). The sweep below
// reaches every object in them; restrictions is empty, so a Restriction's
// shape is left to ref.json's own tests.
func draftDocs() map[string]map[string]any {
	const ts = "2026-10-08T12:00:00Z"
	return map[string]map[string]any{
		"records/failure-signature": {"signature": dgA, "project": prjID, "reason": "gate_test_failed",
			"key": "tests/test_x.py::test_y", "normalizer": map[string]any{"name": "pytest-node", "version": "1"},
			"count": 2, "tasks": []any{tskID}, "evidence": []any{dgA, dgB}, "first_seen": ts, "last_seen": ts},
		"records/claim": {"id": "clm-1", "revision": 2, "supersedes": dgB, "kind": "external_fact", "body": dgA,
			"source": "https://example.org/changelog", "subjects": []any{dgB},
			"evidence": map[string]any{"supporting": []any{dgA}, "refuting": []any{}}, "applicability": []any{"lib >= 2.0"},
			"dependencies":        []any{dgA},
			"validity_conditions": []any{map[string]any{"check": map[string]any{"name": "version-at-least", "version": "1"}, "subject": dgA, "expect": "2.0"}},
			"times":               map[string]any{"observed": ts, "recorded": ts, "valid_from": ts, "valid_until": nil},
			"producer":            map[string]any{"role": "reviewer", "model_snapshot": nil, "attempt": attID},
			"restrictions":        []any{}},
	}
}

// objects lists every JSON object in v, v first, in a fixed order.
func objects(v any) []map[string]any {
	var out []map[string]any
	switch x := v.(type) {
	case map[string]any:
		out = append(out, x)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			out = append(out, objects(x[k])...)
		}
	case []any:
		for _, e := range x {
			out = append(out, objects(e)...)
		}
	}
	return out
}

func TestDraftKnowledgeRecords(t *testing.T) {
	for schema, doc := range draftDocs() {
		if err := valid(t, schema, doc); err != nil {
			t.Errorf("%s: a valid document: %v", schema, err)
		}
	}
	first := func(d map[string]any) { d["revision"], d["supersedes"] = 1, nil }
	inference := func(d map[string]any) { d["kind"], d["source"] = "inference", nil }
	for name, mutate := range map[string]func(map[string]any){"a first revision": first, "an inference without a source": inference} {
		d := draftDocs()["records/claim"]
		mutate(d)
		if err := valid(t, "records/claim", d); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	long := func(n int) string { return strings.Repeat("a", n) }
	type refused struct {
		schema string
		mutate func(map[string]any)
	}
	cases := map[string]refused{
		"a two-line key":                  {"records/failure-signature", func(d map[string]any) { d["key"] = "it failed\nagain" }},
		"a key over 512":                  {"records/failure-signature", func(d map[string]any) { d["key"] = long(513) }},
		"a zero count":                    {"records/failure-signature", func(d map[string]any) { d["count"] = 0 }},
		"no evidence":                     {"records/failure-signature", func(d map[string]any) { d["evidence"] = []any{} }},
		"duplicate evidence":              {"records/failure-signature", func(d map[string]any) { d["evidence"] = []any{dgA, dgA} }},
		"evidence that is no digest":      {"records/failure-signature", func(d map[string]any) { d["evidence"] = []any{"receipt-1"} }},
		"no task":                         {"records/failure-signature", func(d map[string]any) { d["tasks"] = []any{} }},
		"a task that is no task id":       {"records/failure-signature", func(d map[string]any) { d["tasks"] = []any{prjID} }},
		"a project that is no project id": {"records/failure-signature", func(d map[string]any) { d["project"] = tskID }},
		"an unregistered reason":          {"records/failure-signature", func(d map[string]any) { d["reason"] = "flaky" }},
		"a normalizer version over 64":    {"records/failure-signature", func(d map[string]any) { d["normalizer"].(map[string]any)["version"] = long(65) }},
		"a threshold":                     {"records/failure-signature", func(d map[string]any) { d["threshold"] = 3 }},
		"an external fact, no source":     {"records/claim", func(d map[string]any) { d["source"] = nil }},
		"an inference with a source":      {"records/claim", func(d map[string]any) { inference(d); d["source"] = "https://x" }},
		"an observation as a claim":       {"records/claim", func(d map[string]any) { d["kind"], d["source"] = "observation", nil }},
		"no subject":                      {"records/claim", func(d map[string]any) { d["subjects"] = []any{} }},
		"a subject that is no digest":     {"records/claim", func(d map[string]any) { d["subjects"] = []any{"module_a"} }},
		"duplicate supporting evidence":   {"records/claim", func(d map[string]any) { d["evidence"].(map[string]any)["supporting"] = []any{dgA, dgA} }},
		"an attempt that is no attempt":   {"records/claim", func(d map[string]any) { d["producer"].(map[string]any)["attempt"] = prjID }},
		"a decision on the revision": {"records/claim", func(d map[string]any) {
			d["decision"] = map[string]any{"purpose": "review", "state": "accepted", "record": dgA}
		}},
		"inline claim text":                    {"records/claim", func(d map[string]any) { d["body"] = "always use L 2.0" }},
		"a first revision superseding":         {"records/claim", func(d map[string]any) { first(d); d["supersedes"] = dgB }},
		"a later revision superseding nothing": {"records/claim", func(d map[string]any) { d["supersedes"] = nil }},
		"revision 0":                           {"records/claim", func(d map[string]any) { d["revision"] = 0 }},
		"a supersedes that is no digest":       {"records/claim", func(d map[string]any) { d["supersedes"] = "clm-1@1" }},
		"a condition subject that is no digest": {"records/claim", func(d map[string]any) {
			d["validity_conditions"].([]any)[0].(map[string]any)["subject"] = "module_a"
		}},
		"a check version over 64": {"records/claim", func(d map[string]any) {
			d["validity_conditions"].([]any)[0].(map[string]any)["check"].(map[string]any)["version"] = long(65)
		}},
		"an id over 128":            {"records/claim", func(d map[string]any) { d["id"] = long(129) }},
		"a source over 2048":        {"records/claim", func(d map[string]any) { d["source"] = long(2049) }},
		"an applicability over 512": {"records/claim", func(d map[string]any) { d["applicability"] = []any{long(513)} }},
		"a model snapshot over 256": {"records/claim", func(d map[string]any) { d["producer"].(map[string]any)["model_snapshot"] = long(257) }},
		"an expectation over 512": {"records/claim", func(d map[string]any) {
			d["validity_conditions"].([]any)[0].(map[string]any)["expect"] = long(513)
		}},
	}
	// Every object, the document and each nested one, refuses a missing field
	// and an extra one (a prose field included).
	for schema, doc := range draftDocs() {
		for i, obj := range objects(doc) {
			for k := range obj {
				cases[fmt.Sprintf("%s object %d without %s", schema, i, k)] = refused{schema, func(d map[string]any) { delete(objects(d)[i], k) }}
			}
			cases[fmt.Sprintf("%s object %d with a summary", schema, i)] = refused{schema, func(d map[string]any) { objects(d)[i]["summary"] = "x" }}
		}
	}
	for name, c := range cases {
		d := draftDocs()[c.schema]
		c.mutate(d)
		if err := valid(t, c.schema, d); err == nil {
			t.Errorf("%s %s: accepted", c.schema, name)
		}
	}
}
