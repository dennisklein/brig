// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"slices"
	"strings"
	"testing"

	"libvirt.org/go/libvirtxml"
)

func TestMountIDMap(t *testing.T) {
	m, err := MountIDMap(4242, 4343, IDRange{Start: 524288, Count: 65536}, IDRange{Start: 600000, Count: 2000})
	if err != nil {
		t.Fatal(err)
	}
	wantUID := []libvirtxml.DomainFilesystemIDMapEntry{
		{Start: 0, Target: 524288, Count: 1000},
		{Start: 1000, Target: 4242, Count: 1},
		{Start: 1001, Target: 525288, Count: 64536},
	}
	wantGID := []libvirtxml.DomainFilesystemIDMapEntry{
		{Start: 0, Target: 600000, Count: 1000},
		{Start: 1000, Target: 4343, Count: 1},
		{Start: 1001, Target: 601000, Count: 1000},
	}
	if !slices.Equal(m.UID, wantUID) || !slices.Equal(m.GID, wantGID) {
		t.Errorf("map = %+v", m)
	}

	if _, err := MountIDMap(4242, 4343, IDRange{Start: 524288, Count: 1000}, IDRange{Start: 600000, Count: 2000}); err == nil {
		t.Error("a subordinate range without room for the guest's user was accepted")
	}
}

func TestParseSubIDs(t *testing.T) {
	const file = `other:100000:65536
# comment
broken:1:x
alice:524288:65536
alice:700000:65536
`
	for _, c := range []struct {
		name string
		id   uint
		want IDRange
		ok   bool
	}{
		{"alice", 4242, IDRange{Start: 524288, Count: 65536}, true},
		{"bob", 4242, IDRange{}, false},
		{"bob", 0, IDRange{}, false},
	} {
		got, ok, err := parseSubIDs(strings.NewReader(file), c.name, c.id)
		if err != nil || ok != c.ok || got != c.want {
			t.Errorf("%s: got %+v %v %v", c.name, got, ok, err)
		}
	}
	// subuid(5) also allows the user's ID in place of the name.
	got, ok, err := parseSubIDs(strings.NewReader("4242:200000:65536\n"), "alice", 4242)
	if err != nil || !ok || got != (IDRange{Start: 200000, Count: 65536}) {
		t.Errorf("by ID: got %+v %v %v", got, ok, err)
	}
}
