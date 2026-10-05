// SPDX-License-Identifier: Apache-2.0

// Package contract embeds the M0 contract fixtures' goldens
// (fixtures/contract/m0/*.json, schema testing/contract-fixture), so the
// installed adc runs them without the source tree (00 "M0 exit check").
package contract

import "embed"

// Goldens holds m0/<name>.json, one per fixture.
//
//go:embed m0/*.json
var Goldens embed.FS
