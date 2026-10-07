// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/dennisklein/brig/internal/vm"
)

// RepoKeyPath is where the brig repository's OpenPGP key is installed, both
// in the image and in the sandbox mkosi runs dnf in. The gpgkey of
// ImageConfig.RepoFile must point to file://RepoKeyPath.
const RepoKeyPath = "/etc/pki/rpm-gpg/RPM-GPG-KEY-brig"

// repoFilePath is where the brig repository definition is installed.
const repoFilePath = "/etc/yum.repos.d/brig.repo"

// ImageConfig selects what goes into a base image.
type ImageConfig struct {
	// FedoraRelease is the Fedora release number, e.g. 44.
	FedoraRelease int
	// OpenShellVersion pins the OpenShell packages, e.g. "0.1.2"; empty
	// selects the newest version in the brig repository.
	OpenShellVersion string
	// RepoFile is the dnf repository definition of the brig repository.
	RepoFile []byte
	// RepoKey is the OpenPGP public key that signs the brig repository.
	RepoKey []byte
}

// mkosiFiles is the mkosi configuration of the base image. Files ending in
// .tmpl are text/template templates executed with ImageConfig values.
//
//go:embed mkosi
var mkosiFiles embed.FS

// versionRE matches an RPM version, optionally with a release.
var versionRE = regexp.MustCompile(`^[0-9][0-9A-Za-z.+~^_-]*$`)

func (c ImageConfig) validate() error {
	switch {
	case c.FedoraRelease < 1:
		return fmt.Errorf("invalid Fedora release %d", c.FedoraRelease)
	case c.OpenShellVersion != "" && !versionRE.MatchString(c.OpenShellVersion):
		return fmt.Errorf("invalid OpenShell version %q", c.OpenShellVersion)
	case len(c.RepoFile) == 0:
		return errors.New("no brig repository definition")
	case len(c.RepoKey) == 0:
		return errors.New("no brig repository key")
	}
	return nil
}

// openShellPackages lists the OpenShell packages to install, pinned to
// c.OpenShellVersion if set.
func (c ImageConfig) openShellPackages() []string {
	pkgs := []string{"openshell", "openshell-gateway"}
	if c.OpenShellVersion != "" {
		for i := range pkgs {
			pkgs[i] += "-" + c.OpenShellVersion
		}
	}
	return pkgs
}

// WriteMkosiConfig writes a complete mkosi configuration for the base image
// into the empty directory dir. Building it with mkosi yields the disk image
// base.raw and its package manifest base.manifest.
func WriteMkosiConfig(dir string, c ImageConfig) error {
	if err := c.validate(); err != nil {
		return fmt.Errorf("mkosi configuration: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("mkosi configuration: %s is not empty", dir)
	}
	if err := writeMkosiConfig(dir, c); err != nil {
		return fmt.Errorf("writing mkosi configuration: %w", err)
	}
	return nil
}

func writeMkosiConfig(dir string, c ImageConfig) error {
	tree, err := fs.Sub(mkosiFiles, "mkosi")
	if err != nil {
		return err
	}
	err = fs.WalkDir(tree, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if d.IsDir() {
			return mkdirAll(dst)
		}
		data, err := fs.ReadFile(tree, name)
		if err != nil {
			return err
		}
		if base, ok := strings.CutSuffix(dst, ".tmpl"); ok {
			dst = base
			if data, err = render(name, data, c); err != nil {
				return err
			}
		}
		return writeFile(dst, data)
	})
	if err != nil {
		return err
	}
	// dnf reads repositories from mkosi's sandbox while building the image
	// and from the image's /etc inside the VM. mkosi still adds the Fedora
	// repositories, in mkosi.repo, as long as that file does not exist.
	for _, tree := range []string{"mkosi.sandbox", "mkosi.extra"} {
		for name, data := range map[string][]byte{repoFilePath: c.RepoFile, RepoKeyPath: c.RepoKey} {
			dst := filepath.Join(dir, tree, filepath.FromSlash(name))
			if err := mkdirAll(filepath.Dir(dst)); err != nil {
				return err
			}
			if err := writeFile(dst, data); err != nil {
				return err
			}
		}
	}
	return nil
}

func render(name string, text []byte, c ImageConfig) ([]byte, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(string(text))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = t.Execute(&b, map[string]any{
		"FedoraRelease":     c.FedoraRelease,
		"OpenShellPackages": c.openShellPackages(),
	})
	return b.Bytes(), err
}

// mkdirAll is os.MkdirAll with mode 0755 regardless of the umask: mkosi
// copies the modes of the directories it adds to the image.
func mkdirAll(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := mkdirAll(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // G301: image directories are world-readable
		return err
	}
	return os.Chmod(dir, 0o755) //nolint:gosec // G302: image directories are world-readable
}

// writeFile writes a file of the image, executable if it is a script.
func writeFile(path string, data []byte) error {
	perm := fs.FileMode(0o644)
	if bytes.HasPrefix(data, []byte("#!")) {
		perm = 0o755
	}
	return vm.WriteFileAtomic(path, data, perm)
}
