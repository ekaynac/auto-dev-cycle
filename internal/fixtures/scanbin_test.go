// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// The libraries' scans are adc dev scan's (#226 entry 206): the rows that
// run them build this checkout's adc once, into a directory removed when
// the package's tests end.
var scanBin struct {
	once sync.Once
	dir  string
	path string
	err  error
	out  []byte
}

func TestMain(m *testing.M) {
	code := m.Run()
	if scanBin.dir != "" {
		os.RemoveAll(scanBin.dir)
	}
	os.Exit(code)
}

// adcScan: the path of this checkout's adc, built once.
func adcScan(t *testing.T) string {
	t.Helper()
	scanBin.once.Do(func() {
		goBin, err := exec.LookPath("go")
		if err != nil {
			goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
		}
		if scanBin.dir, scanBin.err = os.MkdirTemp("", "adc-scan-bin-"); scanBin.err != nil {
			return
		}
		scanBin.path = filepath.Join(scanBin.dir, "adc")
		cmd := exec.Command(goBin, "build", "-o", scanBin.path, "./cmd/adc")
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=local")
		scanBin.out, scanBin.err = cmd.CombinedOutput()
	})
	if scanBin.err != nil {
		t.Fatalf("building adc for the scans: %v %s", scanBin.err, scanBin.out)
	}
	return scanBin.path
}
