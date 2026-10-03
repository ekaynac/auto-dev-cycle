// SPDX-License-Identifier: Apache-2.0

// Package buildinfo carries the identity every binary embeds: release
// version, source commit and the digest of the schema set it validates
// against (docs/design/m0/01 "Build and CI"). The Makefile sets version and
// commit through -ldflags; the schema-set digest is computed from the
// embedded schema bytes, so it cannot disagree with them.
package buildinfo

import (
	"runtime"

	"github.com/ekaynac/auto-dev-cycle/schemas"
)

// Set by the linker (-X). Defaults identify an unreleased local build.
var (
	Version = "dev"
	Commit  = "unknown"
)

// Info is the build identity in the shape `adc version --json` prints.
type Info struct {
	Binary    string `json:"binary"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	SchemaSet string `json:"schema_set"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns the build identity of the named binary.
func Get(binary string) Info {
	set, err := schemas.SetDigest()
	if err != nil {
		set = "unknown: " + err.Error()
	}
	return Info{
		Binary:    binary,
		Version:   Version,
		Commit:    Commit,
		SchemaSet: set,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String renders the identity on one line for humans.
func (i Info) String() string {
	return i.Binary + " " + i.Version + " (commit " + i.Commit + ", schema set " + i.SchemaSet +
		", " + i.GoVersion + ", " + i.Platform + ")"
}
