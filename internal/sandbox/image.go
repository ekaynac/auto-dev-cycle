// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/ekaynac/auto-dev-cycle/internal/contract"
)

// ManifestLabel is the prepared image's manifest (00 §8 item 2): a JSON
// object naming the toolchain it was prepared for, the sha256 of each
// lockfile its dependencies were baked from and, per platform, the digest
// of the builder's adapter it carries (P22a-5d).
const ManifestLabel = "dev.adc.prepared"

const maxLockfile = 16 << 20

var (
	// ErrImage: the pinned image is absent, its digest differs, or it is
	// built for another OS or architecture than the host's.
	ErrImage = errors.New("sandbox: the pinned image is not present at its digest")
	// ErrManifest: the image was not prepared for this toolchain and base,
	// or carries another adapter than the installed release's.
	ErrManifest = errors.New("sandbox: the image manifest does not match the pinned toolchain and the base's lockfiles")

	pinned = regexp.MustCompile(`^[^@\t\n\f\r ]+@sha256:[0-9a-f]{64}$`)
	imgID  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Image is a pinned image resolved in the host's storage. A run starts the
// resolved ID, so the reference cannot move between check and start. OS
// and Arch are the image's platform: the host's, by Resolve.
type Image struct {
	Pinned   string
	ID       string
	OS, Arch string
	Manifest Manifest
}

// Manifest is what ManifestLabel declares. Adapter is keyed by OS/ARCH
// (one label covers a manifest list; Node differs per platform): the
// AdapterDigest of the closure under /opt/adc/lib. An image prepared
// before P22a-5d has none; CheckManifest does not read it.
type Manifest struct {
	Toolchain string            `json:"toolchain"`
	Lockfiles map[string]string `json:"lockfiles"`         // repo path -> sha256:hex
	Adapter   map[string]string `json:"adapter,omitempty"` // os/arch -> sha256:hex
}

// Resolve finds ref (name@sha256:hex) in the host's storage, never
// pulling: an image whose digest differs from the pin is refused, and so
// is one built for another OS or architecture than the host's (#226 entry
// 88: with an emulator registered, an index digest can sit on another
// platform's image, which would run emulated).
func (h Host) Resolve(ctx context.Context, ref string) (Image, error) {
	// Podman's RepoDigests name the registry (docker.io/library/debian@…);
	// a short name is refused rather than guessed.
	registry, _, qualified := strings.Cut(ref, "/")
	if !pinned.MatchString(ref) || !qualified || !strings.ContainsAny(registry, ".:") && registry != "localhost" {
		return Image{}, fmt.Errorf("%w: not a fully qualified reference pinned by digest", ErrImage)
	}
	out, err := h.podman(ctx, "image", "inspect", "--", ref)
	if err != nil {
		return Image{}, fmt.Errorf("%w: %w", ErrImage, err)
	}
	var found []struct {
		ID           string            `json:"Id"`
		RepoDigests  []string          `json:"RepoDigests"`
		Labels       map[string]string `json:"Labels"`
		OS           string            `json:"Os"`
		Architecture string            `json:"Architecture"`
	}
	if err := json.Unmarshal(out, &found); err != nil || len(found) != 1 || !imgID.MatchString(found[0].ID) ||
		!slices.Contains(found[0].RepoDigests, ref) {
		return Image{}, ErrImage
	}
	if found[0].OS != runtime.GOOS || found[0].Architecture != runtime.GOARCH {
		return Image{}, fmt.Errorf("%w: the image is %s/%s, the host %s/%s", ErrImage, found[0].OS, found[0].Architecture, runtime.GOOS, runtime.GOARCH)
	}
	img := Image{Pinned: ref, ID: found[0].ID, OS: found[0].OS, Arch: found[0].Architecture}
	if raw, ok := found[0].Labels[ManifestLabel]; ok {
		m, err := ParseManifest([]byte(raw))
		if err != nil {
			return Image{}, err
		}
		img.Manifest = m
	}
	return img, nil
}

// ParseManifest reads ManifestLabel's JSON strictly (#373 L2): no
// duplicate key at any level (contract.ReadStrict), and the object's keys
// exactly Manifest's, so encoding/json's case-folded matching never
// merges two of them; nothing else.
func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	doc, err := contract.ReadStrict(raw)
	obj, isObject := doc.(map[string]any)
	if err != nil || !isObject {
		return Manifest{}, fmt.Errorf("%w: the manifest label is not strict JSON", ErrManifest)
	}
	for k := range obj {
		if k != "toolchain" && k != "lockfiles" && k != "adapter" {
			return Manifest{}, fmt.Errorf("%w: the manifest label holds %q", ErrManifest, k)
		}
	}
	if err := contract.UnmarshalStrict(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: the manifest label: %w", ErrManifest, err)
	}
	return m, nil
}

// CheckManifest refuses an image not prepared for toolchain (its key in
// toolchain.images) from exactly the lockfiles at the base, read from
// base, a controller-side copy, never following a symlink.
func (img Image) CheckManifest(toolchain, base string, lockfiles []string) error {
	m := img.Manifest
	if m.Toolchain == "" || m.Toolchain != toolchain {
		return fmt.Errorf("%w: prepared for another toolchain", ErrManifest)
	}
	if !slices.Equal(slices.Sorted(maps.Keys(m.Lockfiles)), slices.Sorted(slices.Values(lockfiles))) {
		return fmt.Errorf("%w: another set of lockfiles", ErrManifest)
	}
	for _, p := range lockfiles {
		sum, err := fileSum(base, p)
		if err != nil {
			return fmt.Errorf("%w: a lockfile at the base is unreadable", ErrManifest)
		}
		if m.Lockfiles[p] != sum {
			return fmt.Errorf("%w: lockfile %s differs at the base", ErrManifest, p)
		}
	}
	return nil
}

// fileSum is the sha256 of rel below dir: a regular file, not a symlink,
// never reached outside dir.
func fileSum(dir, rel string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if st, err := root.Lstat(rel); err != nil || !st.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	r, err := root.Open(rel)
	if err != nil {
		return "", err
	}
	defer r.Close()
	h := sha256.New()
	if n, err := io.Copy(h, io.LimitReader(r, maxLockfile+1)); err != nil || n > maxLockfile {
		return "", errors.New("unreadable or too large")
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
