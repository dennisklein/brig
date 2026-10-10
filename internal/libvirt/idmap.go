// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"

	"libvirt.org/go/libvirtxml"

	"github.com/dennisklein/brig/internal/guest"
)

// IDRange is a range of subordinate IDs from /etc/subuid or /etc/subgid.
type IDRange struct {
	Start, Count uint
}

// MountIDMap returns the ID map of virtiofs mounts: guest.UID, the guest's
// user and group, is the host's uid and gid, so the guest's user can use
// files the host user owns, and what it writes is the host user's. Every
// other guest ID maps to the subordinate IDs subuid and subgid, as with
// libvirt's default map, which maps guest root to the host user instead.
func MountIDMap(uid, gid uint, subuid, subgid IDRange) (*libvirtxml.DomainFilesystemIDMap, error) {
	m := func(what string, id uint, sub IDRange) ([]libvirtxml.DomainFilesystemIDMapEntry, error) {
		if sub.Count <= guest.UID {
			return nil, fmt.Errorf("the %s range of %d IDs must have more than %d", what, sub.Count, guest.UID)
		}
		return []libvirtxml.DomainFilesystemIDMapEntry{
			{Start: 0, Target: sub.Start, Count: guest.UID},
			{Start: guest.UID, Target: id, Count: 1},
			{Start: guest.UID + 1, Target: sub.Start + guest.UID, Count: sub.Count - guest.UID},
		}, nil
	}
	uids, err := m("subordinate UID", uid, subuid)
	if err != nil {
		return nil, err
	}
	gids, err := m("subordinate GID", gid, subgid)
	if err != nil {
		return nil, err
	}
	return &libvirtxml.DomainFilesystemIDMap{UID: uids, GID: gids}, nil
}

// HostMountIDMap returns MountIDMap for the user that runs brig, with the
// first subordinate ID ranges that /etc/subuid and /etc/subgid give it, as
// libvirt picks them for its default map.
func HostMountIDMap() (*libvirtxml.DomainFilesystemIDMap, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	uid, gid := uint(os.Geteuid()), uint(os.Getegid())
	subuid, err := readSubIDs("/etc/subuid", u.Username, uid)
	if err != nil {
		return nil, err
	}
	subgid, err := readSubIDs("/etc/subgid", u.Username, uid)
	if err != nil {
		return nil, err
	}
	return MountIDMap(uid, gid, subuid, subgid)
}

func readSubIDs(file, name string, id uint) (IDRange, error) {
	f, err := os.Open(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return IDRange{}, err
	}
	if err == nil {
		defer func() { _ = f.Close() }()
		r, ok, err := parseSubIDs(f, name, id)
		if err != nil || ok {
			return r, err
		}
	}
	return IDRange{}, fmt.Errorf("%s has no subordinate IDs for %s, which brig needs to share directories with VMs; add them with sudo usermod --add-subuids 524288-589823 --add-subgids 524288-589823 %[2]s, picking a range no other user has", file, name)
}

// parseSubIDs returns the first range of subordinate IDs that a subuid(5)
// or subgid(5) file gives the user name or ID.
func parseSubIDs(r io.Reader, name string, id uint) (IDRange, bool, error) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Split(strings.TrimSpace(s.Text()), ":")
		if len(f) != 3 || (f[0] != name && f[0] != strconv.FormatUint(uint64(id), 10)) {
			continue
		}
		start, err1 := strconv.ParseUint(f[1], 10, 32)
		count, err2 := strconv.ParseUint(f[2], 10, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		return IDRange{Start: uint(start), Count: uint(count)}, true, nil
	}
	return IDRange{}, false, s.Err()
}
