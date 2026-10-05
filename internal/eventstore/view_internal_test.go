// SPDX-License-Identifier: Apache-2.0

package eventstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// A view gives its connection back, whatever fn returns: a kept read
// transaction would hold a connection and pin the write-ahead log.
func TestViewReleasesItsConnection(t *testing.T) {
	e := openEnv(t, filepath.Join(t.TempDir(), "state"))
	for _, ret := range []error{nil, errors.New("fn failed")} {
		_ = e.s.View(context.Background(), func(*Tx) error { return ret })
		if n := e.s.r.Stats().InUse; n != 0 {
			t.Fatalf("after a view returning %v: %d connections in use", ret, n)
		}
	}
}
