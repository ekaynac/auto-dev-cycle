// SPDX-License-Identifier: Apache-2.0
//
// Fence only: the hidden and visible test files under tasks/ belong inside a
// copy of repo/, not to the enclosing repository's Go module. This go.mod keeps
// `go test ./...` and `go vet ./...` at the repository root out of tasks/.
// Nothing is ever built from this module.
module example.com/gomini/fixturetasks

go 1.27.0
