// SPDX-License-Identifier: Apache-2.0

package client

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/ekaynac/auto-dev-cycle/internal/identity"
)

// Refusals of --token-out name the rule, never a token. Each is the
// caller's to correct (a usage error); any other error creating the file
// is the system's (08 "Issued credentials").
var (
	ErrTokenOutDir    = errors.New("--token-out must be in a directory owned by you and not writable by group or others (a symlink is refused)")
	ErrTokenOutExists = errors.New("--token-out exists; it is never replaced: name a new file")
	ErrTokenOutOwn    = errors.New("--token-out names your own token file; an issued token never goes there")
)

// TokenOut is the file a token issued to another actor goes to (08
// "Issued credentials"): never the caller's own token file, whether or not
// it exists. It is created before the command is sent,
// O_CREAT|O_EXCL|O_NOFOLLOW 0600 in a directory checked on its descriptor
// as the token file's is (R5), so nothing is followed or replaced and the
// commit never finds it taken; Discard removes it when no token arrives.
type TokenOut struct {
	dir  int
	name string
	file *os.File
}

// CreateTokenOut reserves path for an issued token.
func CreateTokenOut(path string) (*TokenOut, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dfd, err := unix.Open(filepath.Dir(abs), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrTokenOutDir
	}
	name := filepath.Base(abs)
	var st unix.Stat_t
	if err = unix.Fstat(dfd, &st); err != nil || fileOwner(&st) != os.Getuid() || st.Mode&0o022 != 0 {
		err = ErrTokenOutDir
	} else if ownTokenFile(st, name) {
		err = ErrTokenOutOwn
	}
	if err != nil {
		unix.Close(dfd)
		return nil, err
	}
	fd, err := unix.Openat(dfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		unix.Close(dfd)
		if errors.Is(err, unix.EEXIST) {
			return nil, ErrTokenOutExists
		}
		return nil, fmt.Errorf("creating --token-out: %w", err)
	}
	return &TokenOut{dir: dfd, name: name, file: os.NewFile(uintptr(fd), name)}, nil
}

// ownTokenFile reports whether name in the directory dir describes is the
// caller's token file, by the directory's identity (device and inode), so
// another spelling of the path, or a token file not yet written, is still
// recognised. With no home directory there is no token file to protect.
func ownTokenFile(dir unix.Stat_t, name string) bool {
	own, err := DefaultFiles()
	if err != nil || name != tokenName {
		return false
	}
	var st unix.Stat_t
	return unix.Stat(own.dir(), &st) == nil && st.Dev == dir.Dev && st.Ino == dir.Ino
}

// Write stores tok in the reserved file, 0600 whatever the umask, synced
// with its directory. A failed write, its directory's sync included,
// removes the file, so a token reported undelivered is nowhere on disk.
func (o *TokenOut) Write(tok identity.Secret) error {
	if o.file == nil {
		return errors.New("--token-out was already written or discarded")
	}
	if _, err := identity.ParseSecret(tok.Reveal()); err != nil {
		return ErrTokenMalformed
	}
	file := o.file
	o.file = nil
	defer unix.Close(o.dir)
	err := writeSynced(file, tok)
	if err == nil {
		if err = syncDir(o.dir); err != nil {
			err = fmt.Errorf("syncing the --token-out directory: %w", err)
		}
	}
	if err != nil {
		_ = unix.Unlinkat(o.dir, o.name, 0)
	}
	return err
}

// Discard removes the reserved file unless a token was written to it.
func (o *TokenOut) Discard() {
	if o == nil || o.file == nil {
		return
	}
	_ = o.file.Close()
	o.file = nil
	_ = unix.Unlinkat(o.dir, o.name, 0)
	unix.Close(o.dir)
}
