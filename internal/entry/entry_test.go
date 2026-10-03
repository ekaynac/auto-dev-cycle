// SPDX-License-Identifier: Apache-2.0

package entry

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestMainExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, ExitCouldNotRun},
		{"version", []string{"version"}, ExitOK},
		{"version json", []string{"version", "--json"}, ExitOK},
		{"help", []string{"--help"}, ExitOK},
		{"unknown command", []string{"bogus"}, ExitUsage},
		{"unknown flag", []string{"version", "--yaml"}, ExitUsage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if got := Main("adc", c.args, &out, &errOut); got != c.want {
				t.Errorf("exit %d, want %d (stderr %q)", got, c.want, errOut.String())
			}
			if c.want != ExitOK && errOut.Len() == 0 {
				t.Errorf("non-zero exit with empty stderr")
			}
		})
	}
}

func TestVersionJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Main("adcd", []string{"version", "--json"}, &out, &errOut); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v: %q", err, out.String())
	}
	for _, k := range []string{"binary", "version", "commit", "schema_set", "go_version", "platform"} {
		if got[k] == "" {
			t.Errorf("field %q missing or empty in %v", k, got)
		}
	}
	if got["binary"] != "adcd" {
		t.Errorf("binary = %q, want adcd", got["binary"])
	}
}
