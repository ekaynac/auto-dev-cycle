// SPDX-License-Identifier: Apache-2.0
//
// Fence only: the sealed probe tests under sealed/ belong inside a copy of
// fixtures/t0/go-mini/repo/, not to the enclosing repository's Go module.
// This go.mod keeps `go test ./...` and `go vet ./...` at the repository
// root out of this directory. Nothing is ever built from this module.
module example.com/gomini/reviewqualification

go 1.27.0
