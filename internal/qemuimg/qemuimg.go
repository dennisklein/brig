// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package qemuimg creates, converts, resizes and snapshots the qcow2 disk
// images of brig VMs by running qemu-img.
//
// New images are created with qemu-img's nocow=on option: on btrfs it marks
// them No_COW, which avoids heavy fragmentation from the VM's random writes;
// on other file systems it has no effect. New image files have mode 0600,
// and a failed or cancelled creation leaves no file behind.
//
// Image sizes are rounded up to whole 512-byte sectors: qemu-img does that
// itself when it creates qcow2 images, but refuses to resize them to other
// sizes.
package qemuimg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/dennisklein/brig/internal/bytesize"
)

// binary is the qemu-img executable; tests replace it.
var binary = "qemu-img"

// Create creates an empty qcow2 image with the given virtual size. It refuses
// to overwrite an existing file.
func Create(ctx context.Context, path string, size bytesize.Size) error {
	if size == 0 {
		return errors.New("qemuimg: image size must not be zero")
	}
	size, err := roundSize(size)
	if err != nil {
		return err
	}
	if path, err = absPath(path); err != nil {
		return err
	}
	return newImage(path, func() error {
		_, err := run(ctx, "create", "-f", "qcow2", "-o", "nocow=on", path, sizeArg(size))
		return err
	})
}

// CreateOverlay creates a qcow2 image that records only its differences from
// the qcow2 image base. The overlay stores base's absolute path, and a size
// of zero gives it the size of base; smaller sizes would cut off the end of
// base and are refused. CreateOverlay refuses to overwrite an existing file.
func CreateOverlay(ctx context.Context, path, base string, size bytesize.Size) error {
	size, err := roundSize(size)
	if err != nil {
		return err
	}
	if path, err = absPath(path); err != nil {
		return err
	}
	if base, err = absPath(base); err != nil {
		return err
	}
	return newImage(path, func() error {
		info, err := Inspect(ctx, base)
		if err != nil {
			return err
		}
		if size == 0 {
			size = info.VirtualSize
		}
		if size < info.VirtualSize {
			return fmt.Errorf("overlay size %s is smaller than its base %s (%s)", size, base, info.VirtualSize)
		}
		// QEMU refuses backing files without an explicit format (-F).
		_, err = run(ctx, "create", "-f", "qcow2", "-o", "nocow=on", "-b", base, "-F", "qcow2", path, sizeArg(size))
		return err
	})
}

// Convert converts the raw image src into the new qcow2 image dst. It
// refuses to overwrite an existing file.
func Convert(ctx context.Context, src, dst string) error {
	src, err := absPath(src)
	if err != nil {
		return err
	}
	if dst, err = absPath(dst); err != nil {
		return err
	}
	return newImage(dst, func() error {
		_, err := run(ctx, "convert", "-f", "raw", "-O", "qcow2", "-o", "nocow=on", src, dst)
		return err
	})
}

// Resize grows the qcow2 image at path to size. It refuses to shrink the
// image, which would cut off guest data, and does nothing if the image
// already has that size. The image must not be in use; disks of running VMs
// are resized through libvirt.
func Resize(ctx context.Context, path string, size bytesize.Size) error {
	size, err := roundSize(size)
	if err != nil {
		return err
	}
	if path, err = absPath(path); err != nil {
		return err
	}
	info, err := Inspect(ctx, path)
	if err != nil {
		return err
	}
	if size < info.VirtualSize {
		return fmt.Errorf("cannot shrink %s from %s to %s", path, info.VirtualSize, size)
	}
	if size == info.VirtualSize {
		return nil
	}
	_, err = run(ctx, "resize", "-f", "qcow2", path, sizeArg(size))
	return err
}

// Info describes a disk image.
type Info struct {
	// VirtualSize is the size of the disk the guest sees.
	VirtualSize bytesize.Size
	// Format is the image format, e.g. "qcow2" or "raw".
	Format string
	// BackingFile is the backing file as recorded in the image, or empty.
	BackingFile string
	// Snapshots are the names of the image's internal snapshots.
	Snapshots []string
}

// Inspect describes the image at path. It also works on images that a
// running VM has open, though their details may then be out of date.
func Inspect(ctx context.Context, path string) (Info, error) {
	path, err := absPath(path)
	if err != nil {
		return Info{}, err
	}
	out, err := run(ctx, "info", "--output=json", "-U", path)
	if err != nil {
		return Info{}, err
	}
	// An object of QAPI type ImageInfo (qemu: qapi/block-core.json).
	var v struct {
		VirtualSize uint64 `json:"virtual-size"`
		Format      string `json:"format"`
		BackingFile string `json:"backing-filename"`
		Snapshots   []struct {
			Name string `json:"name"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return Info{}, fmt.Errorf("parsing qemu-img info for %s: %w", path, err)
	}
	info := Info{VirtualSize: bytesize.Size(v.VirtualSize), Format: v.Format, BackingFile: v.BackingFile}
	for _, s := range v.Snapshots {
		info.Snapshots = append(info.Snapshots, s.Name)
	}
	return info, nil
}

// Snapshot saves the current content of the qcow2 image at path as an
// internal snapshot called name. It refuses a name that one of the image's
// snapshots already has, with an error wrapping fs.ErrExist: qcow2 images
// may have several snapshots of the same name, and ApplySnapshot and
// DeleteSnapshot would then act on the oldest. The image must not be in use.
func Snapshot(ctx context.Context, path, name string) error {
	if err := checkSnapshotName(name); err != nil {
		return err
	}
	info, err := Inspect(ctx, path)
	if err != nil {
		return err
	}
	if slices.Contains(info.Snapshots, name) {
		return fmt.Errorf("%s already has a snapshot called %q: %w", path, name, fs.ErrExist)
	}
	return snapshot(ctx, "-c", path, name)
}

// ApplySnapshot reverts the qcow2 image at path to its snapshot called name.
// The snapshot itself is kept. The image must not be in use.
func ApplySnapshot(ctx context.Context, path, name string) error {
	return snapshot(ctx, "-a", path, name)
}

// DeleteSnapshot deletes the snapshot called name from the qcow2 image at
// path. The image must not be in use.
func DeleteSnapshot(ctx context.Context, path, name string) error {
	return snapshot(ctx, "-d", path, name)
}

func snapshot(ctx context.Context, op, path, name string) error {
	if err := checkSnapshotName(name); err != nil {
		return err
	}
	path, err := absPath(path)
	if err != nil {
		return err
	}
	_, err = run(ctx, "snapshot", "-f", "qcow2", op, name, path)
	return err
}

// checkSnapshotName rejects snapshot names that qemu-img would not reliably
// find again: it cuts names off after 255 bytes, and it looks up the
// snapshot to apply by its numeric ID before it looks at names.
func checkSnapshotName(name string) error {
	switch {
	case name == "":
		return errors.New("qemuimg: empty snapshot name")
	case len(name) > 255:
		return errors.New("qemuimg: snapshot name is longer than 255 bytes")
	case strings.Trim(name, "0123456789") == "":
		return fmt.Errorf("qemuimg: snapshot name %q could be mistaken for a snapshot ID", name)
	}
	return nil
}

// run runs a qemu-img subcommand and returns its standard output. Errors
// carry qemu-img's error message, or the context's error if ctx ended the
// run.
func run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	// Keep the terminal's Ctrl-C from qemu-img: ctx decides when it stops,
	// so that a rollback that ignores Ctrl-C can finish.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	switch {
	case err == nil:
		return out, nil
	case ctx.Err() != nil:
		return nil, fmt.Errorf("qemu-img %s: %w", args[0], context.Cause(ctx))
	case errors.Is(err, exec.ErrNotFound):
		return nil, fmt.Errorf("qemu-img is not installed (see brig doctor): %w", err)
	}
	msg := strings.TrimPrefix(strings.TrimSpace(stderr.String()), "qemu-img: ")
	if msg == "" {
		return nil, fmt.Errorf("qemu-img %s: %w", args[0], err)
	}
	return nil, fmt.Errorf("qemu-img %s: %s (%w)", args[0], msg, err)
}

// absPath makes path absolute, so that qemu-img takes it for a file name:
// qemu-img reads a relative name such as "nbd:host:10809" or "json:{...}"
// as a protocol with options, and one that starts with "-" as an option.
func absPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("qemuimg: empty image path")
	}
	return filepath.Abs(path)
}

// newImage creates the empty file path with mode 0600 and then calls write
// to have qemu-img write an image into it. qemu-img itself would silently
// overwrite an existing file, and it creates new files with mode 0644. If
// write fails, newImage removes the file again, so that no partial image
// stays behind. If path exists, even as a dangling symbolic link, newImage
// fails with an error wrapping fs.ErrExist.
func newImage(path string, write func() error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = f.Close()
	if err == nil {
		err = write()
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

// sectorSize is the granularity of qcow2 image sizes.
const sectorSize = 512

// maxSize is the largest image size that qemu-img accepts.
const maxSize = bytesize.Size(math.MaxInt64 &^ (sectorSize - 1))

// roundSize rounds size up to whole sectors.
func roundSize(size bytesize.Size) (bytesize.Size, error) {
	if size > maxSize {
		return 0, fmt.Errorf("qemuimg: image size %s is too large", size)
	}
	return (size + sectorSize - 1) &^ (sectorSize - 1), nil
}

// sizeArg formats size in bytes, which qemu-img takes without a suffix.
func sizeArg(size bytesize.Size) string { return strconv.FormatUint(uint64(size), 10) }
