// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package image builds brig's base images with mkosi and stores them.
//
// Each image lives in its own directory below paths.Dirs.ImagesDir, named
// after the image ID. It holds the read-only disk base.qcow2, the record
// image.json and mkosi's package manifest manifest.json. VM root disks are
// qcow2 overlays backed by base.qcow2, so an image must stay unchanged while
// VMs use it.
package image

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/qemuimg"
	"github.com/dennisklein/brig/internal/vm"
)

// Image is brig's record of a base image.
type Image struct {
	ID            string `json:"id"`
	FedoraRelease int    `json:"fedora_release"`
	// OpenShellVersion is the upstream version of the installed OpenShell
	// gateway, e.g. "0.1.2".
	OpenShellVersion string `json:"openshell_version"`
	// CreatedAt is the build time in UTC, in whole seconds.
	CreatedAt time.Time `json:"created_at"`
}

// ErrNotFound is returned for images that do not exist.
var ErrNotFound = errors.New("no such image")

// MinMkosiVersion is the oldest mkosi that builds brig's images unprivileged.
const MinMkosiVersion = 25

const (
	baseFile     = "base.qcow2"
	recordFile   = "image.json"
	manifestFile = "manifest.json"
	// buildPrefix starts the names of temporary build directories.
	buildPrefix = ".build-"
	// gatewayPackage is the package whose version an image is named after.
	gatewayPackage = "openshell-gateway"
)

// idRE matches image IDs such as f44-openshell0.1.2-20261007T120000Z. The
// version may use every character RPM allows in versions.
var idRE = regexp.MustCompile(`^f[1-9][0-9]*-openshell[0-9A-Za-z._+~^]+-[0-9]{8}T[0-9]{6}Z$`)

func validateID(id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("invalid image ID %q", id)
	}
	return nil
}

// Store keeps base images on disk.
type Store struct {
	root string

	// Seams for tests.
	mkosi       string
	writeConfig func(dir string, c guest.ImageConfig) error
	convert     func(ctx context.Context, src, dst string) error
	now         func() time.Time
}

// NewStore returns a store rooted at dirs.ImagesDir().
func NewStore(dirs paths.Dirs) Store {
	return Store{
		root:        dirs.ImagesDir(),
		mkosi:       "mkosi",
		writeConfig: guest.WriteMkosiConfig,
		convert:     qemuimg.Convert,
		now:         time.Now,
	}
}

func (s Store) dir(id string) string { return filepath.Join(s.root, id) }

// Path is the path of the image's disk, base.qcow2. VM root disks record it
// as their backing file; it is absolute when the store's directories are, as
// those from paths.Default are.
func (s Store) Path(id string) string { return filepath.Join(s.dir(id), baseFile) }

// Get reads the record of image id.
func (s Store) Get(id string) (Image, error) {
	if err := validateID(id); err != nil {
		return Image{}, err
	}
	file := filepath.Join(s.dir(id), recordFile)
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return Image{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Image{}, err
	}
	var img Image
	if err := json.Unmarshal(data, &img); err != nil {
		return Image{}, fmt.Errorf("%s: %w", file, err)
	}
	if img.ID != id {
		return Image{}, fmt.Errorf("%s: record is for image %q", file, img.ID)
	}
	return img, nil
}

// List returns all images, newest first. Directories without a valid record
// are skipped.
func (s Store) List() ([]Image, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var images []Image
	for _, e := range entries {
		if !e.IsDir() || validateID(e.Name()) != nil {
			continue
		}
		img, err := s.Get(e.Name())
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	slices.SortFunc(images, func(a, b Image) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(b.ID, a.ID))
	})
	return images, nil
}

// Newest returns the most recently built image.
func (s Store) Newest() (Image, error) {
	images, err := s.List()
	if err != nil {
		return Image{}, err
	}
	if len(images) == 0 {
		return Image{}, fmt.Errorf("%w: none has been built yet", ErrNotFound)
	}
	return images[0], nil
}

// Remove deletes image id. Callers must make sure that no VM uses it.
func (s Store) Remove(id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	if _, err := os.Stat(s.dir(id)); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	// Drop the record first so that an interrupted removal leaves no
	// half-deleted image behind in List.
	if err := os.Remove(filepath.Join(s.dir(id), recordFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.RemoveAll(s.dir(id))
}

// BuildOptions select what goes into a new image.
type BuildOptions struct {
	// FedoraRelease is the Fedora release number, e.g. 44.
	FedoraRelease int
	// OpenShellVersion pins the OpenShell packages, e.g. "0.1.2"; empty
	// selects the newest version in the brig repository.
	OpenShellVersion string
	// RepoFile and RepoKey are the dnf repository definition of the brig
	// repository and the OpenPGP key that signs it.
	RepoFile, RepoKey []byte
	// Log receives mkosi's output; nil discards it.
	Log io.Writer
	// IfNone makes Build return the newest image instead of building one
	// if the store has any by the time Build holds the build lock, e.g. one
	// that the build it waited for has just added.
	IfNone bool
}

// Build builds a new image with mkosi and adds it to the store. It names the
// image after the Fedora release, the installed OpenShell gateway version
// and the build time. cacheDir is mkosi's package cache, which builds share.
//
// One build runs at a time: Build waits for other builds to finish. mkosi
// runs unprivileged and builds in a temporary directory inside the store, so
// the finished image moves into place atomically; the temporary directory
// is removed when the build fails or is cancelled.
func (s Store) Build(ctx context.Context, cacheDir string, o BuildOptions) (Image, error) {
	if o.FedoraRelease < 1 {
		return Image{}, fmt.Errorf("invalid Fedora release %d", o.FedoraRelease)
	}
	if cacheDir == "" {
		// filepath.Abs would turn it into the working directory.
		return Image{}, errors.New("no package cache directory for mkosi")
	}
	if err := s.checkMkosi(ctx); err != nil {
		return Image{}, err
	}
	cacheDir, err := filepath.Abs(cacheDir)
	if err != nil {
		return Image{}, err
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return Image{}, err
	}
	if err := paths.EnsurePrivate(s.root); err != nil {
		return Image{}, err
	}
	unlock, err := lock(ctx, s.root, o.Log)
	if err != nil {
		return Image{}, err
	}
	defer unlock()
	s.removeStaleBuilds()
	if o.IfNone {
		img, err := s.Newest()
		if err == nil || !errors.Is(err, ErrNotFound) {
			return img, err
		}
	}

	// Build on the file system of the store, so that mkosi and we move files
	// instead of copying them.
	tmp, err := os.MkdirTemp(s.root, buildPrefix)
	if err != nil {
		return Image{}, err
	}
	defer func() { _ = removeAll(tmp) }()
	if tmp, err = filepath.Abs(tmp); err != nil {
		return Image{}, err
	}
	out := filepath.Join(tmp, "output")
	if err := s.runMkosi(ctx, tmp, out, cacheDir, o); err != nil {
		return Image{}, err
	}

	// mkosi names its outputs after Output=base: base.raw for Format=disk
	// and base.manifest for ManifestFormat=json.
	version, err := gatewayVersion(filepath.Join(out, "base.manifest"))
	if err != nil {
		return Image{}, err
	}
	img := Image{
		FedoraRelease:    o.FedoraRelease,
		OpenShellVersion: version,
		CreatedAt:        s.now().UTC().Truncate(time.Second),
	}
	img.ID = fmt.Sprintf("f%d-openshell%s-%s", img.FedoraRelease, version, img.CreatedAt.Format("20060102T150405Z"))
	if err := validateID(img.ID); err != nil {
		return Image{}, err
	}
	if err := s.install(ctx, img, out, filepath.Join(tmp, "image")); err != nil {
		return Image{}, err
	}
	return img, nil
}

// runMkosi writes the mkosi configuration into tmp and builds it into the
// new directory out.
func (s Store) runMkosi(ctx context.Context, tmp, out, cacheDir string, o BuildOptions) error {
	cfg, work := filepath.Join(tmp, "mkosi"), filepath.Join(tmp, "workspace")
	for _, d := range []string{cfg, out, work} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return err
		}
	}
	err := s.writeConfig(cfg, guest.ImageConfig{
		FedoraRelease:    o.FedoraRelease,
		OpenShellVersion: o.OpenShellVersion,
		RepoFile:         o.RepoFile,
		RepoKey:          o.RepoKey,
	})
	if err != nil {
		return fmt.Errorf("writing mkosi configuration: %w", err)
	}

	cmd := exec.CommandContext(ctx, s.mkosi, "-C", cfg, "--output-directory="+out,
		"--package-cache-dir="+cacheDir, "--workspace-directory="+work, "--cache-only=never", "-f", "build")
	cmd.Stdout, cmd.Stderr = o.Log, o.Log
	// On SIGTERM, mkosi stops and removes its workspace.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Minute
	err = cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("mkosi build: %w", context.Cause(ctx))
	}
	if err != nil {
		return fmt.Errorf("mkosi build: %w", err)
	}
	return nil
}

// install assembles image img from mkosi's output in out in the new
// directory dir and moves that into the store.
func (s Store) install(ctx context.Context, img Image, out, dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	base := filepath.Join(dir, baseFile)
	if err := s.convert(ctx, filepath.Join(out, "base.raw"), base); err != nil {
		return err
	}
	// qemu-img convert does not flush its output to disk.
	if err := syncFile(base); err != nil {
		return err
	}
	if err := os.Chmod(base, 0o444); err != nil { //nolint:gosec // G302: the image is not secret, only immutable
		return err
	}
	if err := os.Rename(filepath.Join(out, "base.manifest"), filepath.Join(dir, manifestFile)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(img, "", "  ")
	if err != nil {
		return err
	}
	if err := vm.WriteFileAtomic(filepath.Join(dir, recordFile), append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(dir, s.dir(img.ID))
}

var mkosiVersionRE = regexp.MustCompile(`(\d+)(?:\.\d+)*`)

// checkMkosi makes sure that mkosi is recent enough.
func (s Store) checkMkosi(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, s.mkosi, "--version").Output()
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New("mkosi is not installed (see brig doctor)")
	}
	if err != nil {
		return fmt.Errorf("mkosi --version: %w", err)
	}
	m := mkosiVersionRE.FindStringSubmatch(string(out))
	if m == nil {
		return fmt.Errorf("cannot parse mkosi version %q", strings.TrimSpace(string(out)))
	}
	if major, _ := strconv.Atoi(m[1]); major < MinMkosiVersion {
		return fmt.Errorf("mkosi %s is too old: brig needs mkosi %d or newer", m[0], MinMkosiVersion)
	}
	return nil
}

// manifest is the part of mkosi's JSON manifest that brig reads (mkosi:
// mkosi/manifest.py).
type manifest struct {
	ManifestVersion int `json:"manifest_version"`
	Packages        []struct {
		Name string `json:"name"`
		// Version is [epoch:]version-release for RPM packages.
		Version string `json:"version"`
	} `json:"packages"`
}

// gatewayVersion returns the upstream version of the OpenShell gateway
// package recorded in the mkosi manifest file.
func gatewayVersion(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("reading mkosi manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("%s: %w", file, err)
	}
	if m.ManifestVersion != 1 {
		return "", fmt.Errorf("%s: unsupported manifest version %d", file, m.ManifestVersion)
	}
	for _, p := range m.Packages {
		if p.Name != gatewayPackage {
			continue
		}
		v := p.Version
		if _, rest, ok := strings.Cut(v, ":"); ok {
			v = rest
		}
		v, _, _ = strings.Cut(v, "-")
		if v == "" {
			return "", fmt.Errorf("%s: %s has no version", file, gatewayPackage)
		}
		return v, nil
	}
	return "", fmt.Errorf("%s: %s is not installed in the image", file, gatewayPackage)
}

// lock takes an exclusive lock on dir that lasts until unlock is called,
// waiting while another process holds it.
func lock(ctx context.Context, dir string, log io.Writer) (unlock func(), err error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	for waiting := false; ; waiting = true {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("locking %s: %w", dir, err)
		}
		if !waiting && log != nil {
			fmt.Fprintln(log, "Waiting for another image build to finish...")
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, context.Cause(ctx)
		case <-time.After(time.Second):
		}
	}
}

// removeStaleBuilds removes the build directories of builds that died
// without cleaning up. Only call it while holding the lock.
func (s Store) removeStaleBuilds() {
	entries, _ := os.ReadDir(s.root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), buildPrefix) {
			_ = removeAll(filepath.Join(s.root, e.Name()))
		}
	}
}

// syncFile flushes the content of path to disk.
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// removeAll is os.RemoveAll that also removes trees with read-only
// directories, such as an image tree that an interrupted mkosi left behind.
func removeAll(dir string) error {
	if os.RemoveAll(dir) == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o700) //nolint:gosec // G302 targets files; directories need the execute bit
		}
		return nil
	})
	return os.RemoveAll(dir)
}
