// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"

	"github.com/ekaynac/auto-dev-cycle/internal/buildinfo"
)

// VersionPath reports the daemon's build: version, commit and schema-set
// digest, as `adcd version --json` prints them (01 "Build and CI").
const VersionPath = "/v1/version"

// Version serves VersionPath. It names no actor and reads no store: the
// build identity is what a client checks before it sends credentials.
func Version(w http.ResponseWriter, _ *http.Request) {
	b, err := json.Marshal(buildinfo.Get("adcd"))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(b, '\n'))
}
