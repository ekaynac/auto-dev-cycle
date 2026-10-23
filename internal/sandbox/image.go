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

	pinned = contract.DefPattern("config/project-profile", "PinnedImage") // the schema's rule, one with dispatch.IsImageSignal (Opus #425 L2)
	imgID  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Pinned reports whether ref is a pinned image reference: the profile
// schema's PinnedImage, the one rule (Resolve's, and the launch's image
// signal's, dispatch.IsImageSignal).
func Pinned(ref string) bool { return pinned.MatchString(ref) }

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

// ErrUnqualified: the pin names no registry, or is no digest pin: a fault
// of the profile that pins it, which no wait clears (Opus #393 L2). It is an
// ErrImage.
var ErrUnqualified = fmt.Errorf("%w: not a fully qualified reference pinned by digest", ErrImage)

// Resolve finds ref (name@sha256:hex) in the host's storage, never
// pulling: an image whose digest differs from the pin is refused, and so
// is one built for another OS or architecture than the host's (#226 entry
// 88: with an emulator registered, an index digest can sit on another
// platform's image, which would run emulated). A host that holds several
// platforms' images under an index pin may inspect it as another
// platform's (podman 4.9): Resolve then takes, among the images whose
// RepoDigests hold ref, the one of the host's platform, and refuses none
// or more than one (Opus #397 L1).
func (h Host) Resolve(ctx context.Context, ref string) (Image, error) {
	// Podman's RepoDigests name the registry (docker.io/library/debian@…);
	// a short name is refused rather than guessed.
	registry, _, qualified := strings.Cut(ref, "/")
	if !pinned.MatchString(ref) || !qualified || !strings.ContainsAny(registry, ".:") && registry != "localhost" {
		return Image{}, ErrUnqualified
	}
	found, err := h.inspect(ctx, ref, ref)
	if err != nil {
		return Image{}, err
	}
	if !found[0].host() {
		other := found[0]
		if found, err = h.underPin(ctx, ref); err != nil {
			return Image{}, err
		}
		found = slices.DeleteFunc(found, func(i inspected) bool { return !i.host() })
		switch len(found) {
		case 0:
			return Image{}, fmt.Errorf("%w: the image is %s/%s, the host %s/%s", ErrImage, other.OS, other.Architecture, runtime.GOOS, runtime.GOARCH)
		case 1:
		default:
			return Image{}, fmt.Errorf("%w: %d images of the host's platform hold %s", ErrImage, len(found), ref)
		}
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

// inspected is what Resolve reads of an image (podman image inspect).
type inspected struct {
	ID           string            `json:"Id"`
	RepoDigests  []string          `json:"RepoDigests"`
	Labels       map[string]string `json:"Labels"`
	OS           string            `json:"Os"`
	Architecture string            `json:"Architecture"`
}

func (i inspected) host() bool { return i.OS == runtime.GOOS && i.Architecture == runtime.GOARCH }

// inspect inspects names, each of which must be an image whose
// RepoDigests hold ref.
func (h Host) inspect(ctx context.Context, ref string, names ...string) ([]inspected, error) {
	out, err := h.podman(ctx, append([]string{"image", "inspect", "--"}, names...)...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImage, err)
	}
	var found []inspected
	if err := json.Unmarshal(out, &found); err != nil || len(found) != len(names) {
		return nil, ErrImage
	}
	for _, i := range found {
		if !imgID.MatchString(i.ID) || !slices.Contains(i.RepoDigests, ref) {
			return nil, ErrImage
		}
	}
	return found, nil
}

// underPin inspects every image whose RepoDigests hold ref (podman images:
// one row per name, so an image's ID once).
func (h Host) underPin(ctx context.Context, ref string) ([]inspected, error) {
	out, err := h.podman(ctx, "images", "--all", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImage, err)
	}
	var listed []struct {
		ID          string   `json:"Id"`
		RepoDigests []string `json:"RepoDigests"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, fmt.Errorf("%w: podman's images: %w", ErrImage, err)
	}
	var ids []string
	for _, l := range listed {
		if slices.Contains(l.RepoDigests, ref) && !slices.Contains(ids, l.ID) {
			ids = append(ids, l.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return h.inspect(ctx, ref, ids...)
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
	if err := img.CheckPrepared(toolchain, lockfiles); err != nil {
		return err
	}
	m := img.Manifest
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

// CheckPrepared is CheckManifest without a base: the image is prepared
// for toolchain and for exactly lockfiles, none of which is hashed. It is
// a run's with no base checked out (a reviewer's empty workspace; Mythos
// chunk 4a H1).
func (img Image) CheckPrepared(toolchain string, lockfiles []string) error {
	m := img.Manifest
	if m.Toolchain == "" || m.Toolchain != toolchain {
		return fmt.Errorf("%w: prepared for another toolchain", ErrManifest)
	}
	if !slices.Equal(slices.Sorted(maps.Keys(m.Lockfiles)), slices.Sorted(slices.Values(lockfiles))) {
		return fmt.Errorf("%w: another set of lockfiles", ErrManifest)
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
