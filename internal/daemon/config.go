// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ekaynac/auto-dev-cycle/gen/go/types"
	"github.com/ekaynac/auto-dev-cycle/internal/capacity"
	"github.com/ekaynac/auto-dev-cycle/internal/contract"
	"github.com/ekaynac/auto-dev-cycle/internal/eventstore"
	"github.com/ekaynac/auto-dev-cycle/internal/identity"
	"github.com/ekaynac/auto-dev-cycle/internal/registry"
	"github.com/ekaynac/auto-dev-cycle/internal/sandbox"
)

// Config is what the daemon runs with, validated at load and again by Start.
type Config struct {
	StateDir    string                // required: absolute and clean; the private state directory (03)
	Groups      []types.ResourceGroup // what capacity reserves from; none by default
	TrustedHead *eventstore.Head      // optional: the log must reach it and carry its hash (03 rule 3)
	Shared      *SharedSocket         // optional, off by default (14 R2)
	HTTPS       *HTTPSListener        // optional, off by default (14 R2, R3)
	Sandbox     *sandbox.Host         // optional (--sandbox-root): the probe route's sandbox (P9)
	// Registry (--registry, optional) is the executors, checked against
	// Groups (P7); registryErr is its file's refusal, reported at load.
	Registry    *types.ExecutorRegistry
	registryErr error
	// clock is the store clock (eventstore.Config.Now) that every command
	// and the bootstrap decide by; nil is time.Now. Only tests set it.
	clock func() time.Time
	// authenticated is the command endpoint's test hook (commands).
	authenticated func(identity.Principal)
	// reconcileEvery is the reconcile loop's period; 0 is reconcileEvery
	// (5 s). Only tests set it (Opus #312 L2).
	reconcileEvery time.Duration
}

// SharedSocket enables the shared socket, <RunDir>/adcd-shared.sock, mode
// 0660 with group Group, for supervisors under other OS users (08). The
// run directory is the daemon's: owned by its uid, of Group, searchable
// by the group and writable by no one else, so no supervisor can replace
// the socket.
type SharedSocket struct {
	RunDir string // absolute and clean, outside the state directory
	Group  string // a group name or numeric gid
}

// gid resolves the group: a numeric gid as it is, else a group name.
func (s SharedSocket) gid() (int, error) {
	if n, err := strconv.Atoi(s.Group); err == nil && n >= 0 {
		return n, nil
	}
	g, err := user.LookupGroup(s.Group)
	if err == nil {
		var n int
		if n, err = strconv.Atoi(g.Gid); err == nil {
			return n, nil
		}
	}
	return -1, configError("the shared group does not exist")
}

// ErrConfig marks a refused configuration. Its messages are fixed text:
// they never echo a path or a value read from a file.
var ErrConfig = errors.New("adcd: configuration")

const (
	maxGroupsFile = 1 << 20 // bytes
	groupSchema   = "records/resource-group"
	usage         = "usage: adcd run --state-dir DIR [--resource-groups FILE [--registry FILE]] [--trusted-head SEQ:sha256:HEX] [--shared-run-dir DIR --shared-group GROUP] [--https-addr IP:PORT --https-cert FILE --https-key FILE] [--sandbox-root DIR [--podman PATH]]"
	// sandboxRefusal is the one text for a refused sandbox (P9b).
	sandboxRefusal = "the sandbox: --podman and --sandbox-root must be absolute, clean paths; the root short " +
		"(Podman's runroot, <root>/r, is at most 50 characters) and apart from the state directory and the sockets; " +
		"<root>/runs a directory on its own filesystem, apart from theirs and the home's; and the home not /home/adc. " +
		"Run limits also need cgroup delegation: under a systemd service, Delegate=yes"
)

var headHash = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func configError(detail string) error { return fmt.Errorf("%w: %s", ErrConfig, detail) }

// LoadConfig parses the `adcd run` flags.
func LoadConfig(args []string) (Config, error) {
	return loadConfig("adcd run", usage, args, func(fs *flag.FlagSet) func(*Config) {
		runDir, group := fs.String("shared-run-dir", "", ""), fs.String("shared-group", "", "")
		addr, cert, key := fs.String("https-addr", "", ""), fs.String("https-cert", "", ""), fs.String("https-key", "", "")
		root, podman := fs.String("sandbox-root", "", ""), fs.String("podman", "/usr/bin/podman", "")
		reg := fs.String("registry", "", "")
		return func(cfg *Config) {
			if *reg != "" {
				doc, err := registry.Load(*reg)
				cfg.Registry, cfg.registryErr = &doc, err
			}
			if *runDir != "" || *group != "" {
				cfg.Shared = &SharedSocket{RunDir: *runDir, Group: *group}
			}
			if *addr != "" || *cert != "" || *key != "" {
				cfg.HTTPS = &HTTPSListener{Addr: *addr, CertFile: *cert, KeyFile: *key}
			}
			if *root != "" {
				cfg.Sandbox = &sandbox.Host{Podman: *podman, Root: *root, Forbidden: cfg.controllerPaths(), Home: home()}
			}
		}
	})
}

// controllerPaths are what no sandbox run may mount or see: the state
// directory (with adcd's private socket), the shared socket and the
// Podman API sockets.
func (c Config) controllerPaths() []string {
	out := []string{filepath.Clean(c.StateDir), fmt.Sprintf("/run/user/%d/podman/podman.sock", os.Getuid()), "/run/podman/podman.sock"}
	if c.Shared != nil {
		out = append(out, filepath.Join(c.Shared.RunDir, sharedSocket))
	}
	return out
}

// home is the daemon user's home from the user database; with none, ""
// (refused).
func home() string {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return filepath.Clean(u.HomeDir)
	}
	return ""
}

// loadConfig parses the flags of every command that opens the store, and
// those more registers; what more returns applies them.
func loadConfig(name, usage string, args []string, more func(*flag.FlagSet) func(*Config)) (Config, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	state, groupsFile, trusted := fs.String("state-dir", "", ""), fs.String("resource-groups", "", ""), fs.String("trusted-head", "", "")
	apply := more(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return Config{}, configError("unknown flag, missing value or extra argument; " + usage)
	}
	cfg := Config{StateDir: *state, Groups: []types.ResourceGroup{}}
	apply(&cfg)
	if cfg.registryErr != nil {
		return Config{}, configError(cfg.registryErr.Error())
	}
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
	case c.Shared != nil && (c.Shared.RunDir == "" || c.Shared.Group == ""):
		return configError("the shared socket needs both --shared-run-dir and --shared-group")
	case c.Shared != nil && (!absClean(c.Shared.RunDir) || c.Shared.RunDir == c.StateDir || strings.HasPrefix(c.Shared.RunDir, c.StateDir+"/")):
		return configError("the shared run directory must be an absolute, clean path apart from the state directory")
	}
	if c.Shared != nil {
		if _, err := c.Shared.gid(); err != nil {
			return err
		}
	}
	if c.HTTPS != nil {
		if err := c.HTTPS.validate(absClean); err != nil {
			return err
		}
	}
	if c.Sandbox != nil && c.Sandbox.Validate() != nil {
		return configError(sandboxRefusal)
	}
	for i, g := range c.Groups {
		if _, err := contract.Encode(groupSchema, g); err != nil {
			return configError(fmt.Sprintf("resource group %d does not satisfy %s", i, groupSchema))
		}
	}
	if _, err := capacity.NewReservations(c.Groups); err != nil {
		return configError("the resource groups break a capacity rule or use one not wired yet")
	}
	if c.Registry != nil {
		if _, err := registry.New(*c.Registry, c.Groups); err != nil {
			return configError(err.Error()) // fixed text: a rule and an index
		}
	}
	return nil
}

// readGroups reads a JSON array of resource groups, each checked against
// its schema as written, so no field is dropped by decoding.
func readGroups(path string) ([]types.ResourceGroup, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, configError("the resource-group file cannot be opened")
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, configError("the resource-group file is not a regular file")
	}
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
