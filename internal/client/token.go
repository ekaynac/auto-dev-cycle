// SPDX-License-Identifier: Apache-2.0

package client

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// TokenEnv names the variable that, when set, holds the token instead of
// the file (04 "Actors and tokens").
const TokenEnv = "ADC_TOKEN"

const (
	tokenName     = "token"
	maxTokenBytes = 4096
)

// Refusals name the file, never its content [R5].
var (
	ErrNoToken         = errors.New("no token: set ADC_TOKEN or write ~/.config/adc/token")
	ErrTokenMalformed  = errors.New("the token is not in the issued form")
	ErrTokenDir        = errors.New("~/.config/adc must be a directory owned by you and not writable by group or others (a symlink is refused)")
	ErrTokenNotRegular = errors.New("~/.config/adc/token must be a regular file (a symlink is refused)")
	ErrTokenOwner      = errors.New("~/.config/adc/token must be owned by you")
	ErrTokenMode       = errors.New("~/.config/adc/token must have mode 0600")
)

// Files is where the CLI keeps its token: <Home>/.config/adc/token, owned
// by UID. The home directory and ~/.config are the user's and trusted as
// they resolve; adc and token are never followed.
type Files struct {
	Home string
	UID  int
}

// DefaultFiles is the current user's.
func DefaultFiles() (Files, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Files{}, err
	}
	return Files{Home: home, UID: os.Getuid()}, nil
}

func (f Files) dir() string { return filepath.Join(f.Home, ".config", "adc") }

// Token reads ADC_TOKEN when set, the current user's token file otherwise.
func Token(getenv func(string) string) (identity.Secret, error) {
	if v := getenv(TokenEnv); v != "" {
		return parseToken(v)
	}
	f, err := DefaultFiles()
	if err != nil {
		return identity.Secret{}, err
	}
	return f.Load()
}

func parseToken(s string) (identity.Secret, error) {
	tok, err := identity.ParseSecret(s)
	if err != nil {
		return identity.Secret{}, ErrTokenMalformed
	}
	return tok, nil
}

// openDir opens the token directory without following a symlink and checks
// it on the descriptor: a directory owned by UID, not group/other writable.
func (f Files) openDir() (int, error) {
	fd, err := unix.Open(f.dir(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return -1, ErrNoToken
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return -1, ErrTokenDir
	case err != nil:
		return -1, fmt.Errorf("opening ~/.config/adc: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("checking ~/.config/adc: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || int(st.Uid) != f.UID || st.Mode&0o022 != 0 {
		unix.Close(fd)
		return -1, ErrTokenDir
	}
	return fd, nil
}

// Load reads the token file [R5]: opened without following a symlink (or
// blocking on a FIFO), then checked on the opened descriptor.
func (f Files) Load() (identity.Secret, error) {
	dfd, err := f.openDir()
	if err != nil {
		return identity.Secret{}, err
	}
	defer unix.Close(dfd)
	fd, err := unix.Openat(dfd, tokenName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return identity.Secret{}, ErrNoToken
	case errors.Is(err, unix.ELOOP):
		return identity.Secret{}, ErrTokenNotRegular
	case err != nil:
		return identity.Secret{}, fmt.Errorf("opening ~/.config/adc/token: %w", err)
	}
	file := os.NewFile(uintptr(fd), tokenName)
	defer file.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return identity.Secret{}, fmt.Errorf("checking ~/.config/adc/token: %w", err)
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return identity.Secret{}, ErrTokenNotRegular
	case fileOwner(&st) != f.UID:
		return identity.Secret{}, ErrTokenOwner
	case st.Mode&0o7777 != 0o600:
		return identity.Secret{}, ErrTokenMode
	}
	b, err := io.ReadAll(io.LimitReader(file, maxTokenBytes+1))
	if err != nil {
		return identity.Secret{}, fmt.Errorf("reading ~/.config/adc/token: %w", err)
	}
	if len(b) > maxTokenBytes {
		return identity.Secret{}, ErrTokenMalformed
	}
	return parseToken(strings.TrimSuffix(string(b), "\n"))
}

// fileOwner is the token file's owner; a test replaces it, since it cannot
// chown.
var fileOwner = func(st *unix.Stat_t) int { return int(st.Uid) }

// tempName names the file a token is written to before its rename; tests
// replace it.
var tempName = func() string { return tokenName + ".tmp-" + rand.Text() }

// Write stores a token for bootstrap (4b): it creates ~/.config/adc 0700
// if absent (and checks it either way), writes a new file opened
// O_CREAT|O_EXCL|O_NOFOLLOW 0600 in it, syncs it and renames it over the
// token, so a reader sees the old token or the new one, and no existing
// path is ever written through.
func (f Files) Write(tok identity.Secret) error {
	if _, err := identity.ParseSecret(tok.Reveal()); err != nil {
		return ErrTokenMalformed
	}
	if err := os.Mkdir(filepath.Join(f.Home, ".config"), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("creating ~/.config: %w", err)
	}
	if err := unix.Mkdir(f.dir(), 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("creating ~/.config/adc: %w", err)
	}
	dfd, err := f.openDir()
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	tmp := tempName()
	fd, err := unix.Openat(dfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("creating the new token file: %w", err)
	}
	if err := writeSynced(os.NewFile(uintptr(fd), tmp), tok); err != nil {
		_ = unix.Unlinkat(dfd, tmp, 0)
		return err
	}
	if err := unix.Renameat(dfd, tmp, dfd, tokenName); err != nil {
		_ = unix.Unlinkat(dfd, tmp, 0)
		return fmt.Errorf("replacing ~/.config/adc/token: %w", err)
	}
	if err := unix.Fsync(dfd); err != nil {
		return fmt.Errorf("syncing ~/.config/adc: %w", err)
	}
	return nil
}

// writeSynced fixes the mode at 0600 whatever the umask, writes the token
// and syncs it, then closes the file.
func writeSynced(file *os.File, tok identity.Secret) error {
	err := file.Chmod(0o600)
	if err == nil {
		_, err = file.WriteString(tok.Reveal() + "\n")
	}
	if err == nil {
		err = file.Sync()
	}
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing the new token file: %w", err)
	}
	return nil
}
