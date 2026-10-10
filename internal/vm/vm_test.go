// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vm

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/paths"
)

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "dev", "dev-2", "a1", "abcdefghijklmnopqrstuvwxyz01234"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Dev", "1dev", "dev-", "-dev", "dev_2", "dev.2", "a/b", "..", "abcdefghijklmnopqrstuvwxyz012345"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) succeeded", bad)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(paths.Dirs{Data: t.TempDir()})
	if vms, err := s.List(); err != nil || len(vms) != 0 {
		t.Fatalf("List() on empty store = %v, %v", vms, err)
	}
	v := &VM{
		Name: "dev", UUID: "8a0e3f4b-1d2c-4e5f-9a8b-7c6d5e4f3a2b",
		CreatedAt: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
		Image:     "f44-0.1.2-20261007T120000Z", CPUs: 4, Memory: 8 * bytesize.GiB,
		RootDisk: 20 * bytesize.GiB, DataDisk: 40 * bytesize.GiB, NetworkProfile: "default",
		Mounts: []Mount{{Source: "/src", Target: "/mnt/src", ReadOnly: true, Tag: "brig0"}},
		Ports:  Ports{SSH: 40022, Gateway: 40670},
	}
	if err := s.Save(v); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, v) {
		t.Fatalf("Load() = %+v, want %+v", got, v)
	}
	fi, err := os.Stat(filepath.Join(s.Dir("dev"), "vm.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("vm.json mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if names := s.Names(); !reflect.DeepEqual(names, []string{"dev"}) {
		t.Fatalf("Names() = %v", names)
	}
	if err := s.Remove("dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load() after Remove = %v, want ErrNotFound", err)
	}
}

func TestListSkipsForeignDirectories(t *testing.T) {
	s := NewStore(paths.Dirs{Data: t.TempDir()})
	for _, d := range []string{"Not-A-VM", "empty"} {
		if err := os.MkdirAll(s.Dir(d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	vms, err := s.List()
	if err != nil || len(vms) != 0 {
		t.Fatalf("List() = %v, %v", vms, err)
	}
}

func TestListKeepsGoodRecordsBesideBrokenOnes(t *testing.T) {
	s := NewStore(paths.Dirs{Data: t.TempDir()})
	for _, name := range []string{"a", "c"} {
		if err := s.Save(&VM{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(s.Dir("b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.recordPath("b"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	vms, err := s.List()
	if err == nil || len(vms) != 2 || vms[0].Name != "a" || vms[1].Name != "c" {
		t.Fatalf("List() = %v, %v; want a and c and an error about b", vms, err)
	}
	if names := s.Names(); !reflect.DeepEqual(names, []string{"a", "c"}) {
		t.Fatalf("Names() = %v", names)
	}
}

func TestParseMount(t *testing.T) {
	wd, _ := os.Getwd()
	for arg, want := range map[string]Mount{
		"/src/foo":                      {Source: "/src/foo", Target: "/mnt/foo", ReadOnly: true},
		"/src/foo:/work":                {Source: "/src/foo", Target: "/work", ReadOnly: true},
		"/src/foo:rw":                   {Source: "/src/foo", Target: "/mnt/foo"},
		"/src/foo:/work:rw":             {Source: "/src/foo", Target: "/work"},
		"/src/foo:/work:sandbox":        {Source: "/src/foo", Target: "/work", ReadOnly: true, Sandbox: true},
		"/src/foo:ro,sandbox":           {Source: "/src/foo", Target: "/mnt/foo", ReadOnly: true, Sandbox: true},
		"rel":                           {Source: filepath.Join(wd, "rel"), Target: "/mnt/rel", ReadOnly: true},
		"/src/foo:/home/agent/work:ro,": {Source: "/src/foo", Target: "/home/agent/work", ReadOnly: true},
		"/src/café":                     {Source: "/src/café", Target: "/mnt/café", ReadOnly: true},
	} {
		got, err := ParseMount(arg)
		if err != nil {
			t.Errorf("ParseMount(%q): %v", arg, err)
			continue
		}
		if got != want {
			t.Errorf("ParseMount(%q) = %+v, want %+v", arg, got, want)
		}
	}
	for _, bad := range []string{"", ":/x", "/a:/b:/c:d", "/a:/b:bogus", "/a:rw,sandbox", "/a:/", "/a:/b/../c", "/a:/b c", "/a:/b,sandbox", "/a:/b,rw:ro", "/src/caf\xe9", "/src/ctl\x01dir", "/src/a\nb", "/src/a\uFFFEb"} {
		if _, err := ParseMount(bad); err == nil {
			t.Errorf("ParseMount(%q) succeeded", bad)
		}
	}
}

func TestAssignMountTags(t *testing.T) {
	tags := func(v *VM) []string {
		var tags []string
		for _, m := range v.Mounts {
			tags = append(tags, m.Tag)
		}
		return tags
	}
	v := &VM{Mounts: []Mount{{Tag: "brig1"}, {}, {}}}
	v.AssignMountTags()
	if got, want := tags(v), []string{"brig1", "brig2", "brig3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}
	// Tags of removed mounts stay retired.
	v.Mounts = append(v.Mounts[:2], Mount{})
	v.AssignMountTags()
	if got, want := tags(v), []string{"brig1", "brig2", "brig4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}
	v.Mounts = []Mount{{}}
	v.AssignMountTags()
	if got, want := tags(v), []string{"brig5"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}
}

func TestLoadRejectsWritableSandboxMount(t *testing.T) {
	s := NewStore(paths.Dirs{Data: t.TempDir()})
	v := &VM{Name: "dev", Mounts: []Mount{{Source: "/src", Target: "/mnt/src", Sandbox: true, Tag: "brig0"}}}
	if err := s.Save(v); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("dev"); err == nil {
		t.Fatal("Load() accepted a writable sandbox mount")
	}
}
