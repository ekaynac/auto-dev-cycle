// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
)

// Config is what the daemon runs with, validated at load and again by Start.
type Config struct {
	StateDir    string                // required: absolute and clean; the private state directory (03)
	Groups      []types.ResourceGroup // what capacity reserves from; none by default
	TrustedHead *eventstore.Head      // optional: the log must reach it and carry its hash (03 rule 3)
}

// ErrConfig marks a refused configuration. Its messages are fixed text:
// they never echo a path or a value read from a file.
var ErrConfig = errors.New("adcd: configuration")

const (
	maxGroupsFile = 1 << 20 // bytes
	groupSchema   = "records/resource-group"
	usage         = "usage: adcd run --state-dir DIR [--resource-groups FILE] [--trusted-head SEQ:sha256:HEX]"
)

var headHash = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func configError(detail string) error { return fmt.Errorf("%w: %s", ErrConfig, detail) }

// LoadConfig parses the `adcd run` flags.
func LoadConfig(args []string) (Config, error) {
	fs := flag.NewFlagSet("adcd run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	state, groupsFile, trusted := fs.String("state-dir", "", ""), fs.String("resource-groups", "", ""), fs.String("trusted-head", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return Config{}, configError("unknown flag, missing value or extra argument; " + usage)
	}
	cfg := Config{StateDir: *state, Groups: []types.ResourceGroup{}}
	if *trusted != "" {
		seq, hash, _ := strings.Cut(*trusted, ":")
		n, err := strconv.ParseInt(seq, 10, 64)
		if err != nil {
			return Config{}, configError("--trusted-head is not SEQ:sha256:HEX")
		}
		cfg.TrustedHead = &eventstore.Head{Seq: n, Hash: hash}
	}
	if *groupsFile != "" {
		var err error
		if cfg.Groups, err = readGroups(*groupsFile); err != nil {
			return Config{}, err
		}
	}
	return cfg, cfg.Validate()
}

// Validate checks the state directory, the trusted head's syntax, and the
// resource groups: each against
// its schema, then the capacity rules, which refuse any group whose
// semantics are not wired yet rather than dropping them.
func (c Config) Validate() error {
	absClean := func(p string) bool { return filepath.IsAbs(p) && filepath.Clean(p) == p }
	switch {
	case c.StateDir == "":
		return configError("--state-dir is required")
	case !absClean(c.StateDir):
		return configError("the state directory must be an absolute, clean path")
	case c.TrustedHead != nil && (c.TrustedHead.Seq < 0 || !headHash.MatchString(c.TrustedHead.Hash)):
		return configError("the trusted head is not a non-negative seq and a sha256:HEX hash")
	}
	for i, g := range c.Groups {
		if _, err := contract.Encode(groupSchema, g); err != nil {
			return configError(fmt.Sprintf("resource group %d does not satisfy %s", i, groupSchema))
		}
	}
	if _, err := capacity.NewReservations(c.Groups); err != nil {
		return configError("the resource groups break a capacity rule or use one not wired yet")
	}
	return nil
}

// readGroups reads a JSON array of resource groups, each checked against
// its schema as written, so no field is dropped by decoding.
func readGroups(path string) ([]types.ResourceGroup, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, configError("the resource-group file cannot be opened")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxGroupsFile+1))
	if err != nil || len(raw) > maxGroupsFile {
		return nil, configError("the resource-group file cannot be read, or is larger than 1 MiB")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, configError("the resource-group file is not a JSON array")
	}
	groups := make([]types.ResourceGroup, len(items))
	for i, item := range items {
		if err := contract.Validate(groupSchema, item); err != nil || json.Unmarshal(item, &groups[i]) != nil {
			return nil, configError(fmt.Sprintf("resource group %d does not satisfy %s", i, groupSchema))
		}
	}
	return groups, nil
}
