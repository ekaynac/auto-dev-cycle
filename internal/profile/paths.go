// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// checkRepoPath: a relative, slash-separated, clean path inside the
// repository, without NUL.
func checkRepoPath(p string) error {
	switch {
	case strings.ContainsRune(p, 0):
		return errors.New("a path has no NUL byte")
	case strings.ContainsRune(p, '\\'):
		return errors.New("use forward slashes")
	case strings.HasPrefix(p, "/"):
		return errors.New("must be relative to the repository root")
	case p == "." || path.Clean(p) != p:
		return fmt.Errorf("must be clean (%q)", path.Clean(p))
	case p == ".." || strings.HasPrefix(p, "../"):
		return errors.New("must stay inside the repository")
	}
	return nil
}
