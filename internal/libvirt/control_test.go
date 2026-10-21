// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
)

func (f *fakeClient) DomainScreenshot(d golibvirt.Domain, w io.Writer, screen, flags uint32) (golibvirt.OptString, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("screenshot %s %d %d", d.Name, screen, flags)
	fd, err := f.dom(d)
	if err != nil {
		return nil, err
	}
	// Like go-libvirt, stream in chunks.
	for b := fd.screen; len(b) > 0; {
		n := min(len(b), 1<<20)
		if _, err := w.Write(b[:n]); err != nil {
			return nil, err
		}
		b = b[n:]
	}
	return golibvirt.OptString{"image/x-portable-pixmap"}, nil
}

func (f *fakeClient) DomainSendKey(d golibvirt.Domain, codeset, hold uint32, codes []uint32, flags uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("sendkey %s %d %d %v %d", d.Name, codeset, hold, codes, flags)
	_, err := f.dom(d)
	return err
}

func (f *fakeClient) QEMUDomainMonitorCommand(d golibvirt.Domain, cmd string, flags uint32) (string, error) {
	f.mu.Lock()
	f.record("monitor %s %s %d", d.Name, cmd, flags)
	_, err := f.dom(d)
	reply := f.reply
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	return reply(cmd)
}

func (f *fakeClient) QEMUDomainAgentCommand(d golibvirt.Domain, cmd string, timeout int32, flags uint32) (golibvirt.OptString, error) {
	f.mu.Lock()
	f.record("agent %s %s %d %d", d.Name, cmd, timeout, flags)
	_, err := f.dom(d)
	reply := f.reply
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out, err := reply(cmd)
	if err != nil {
		return nil, err
	}
	return golibvirt.OptString{out}, nil
}

func (f *fakeClient) DomainSnapshotCreateXML(d golibvirt.Domain, x string, flags uint32) (golibvirt.DomainSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("snapshot-create %s %s %d", d.Name, x, flags)
	fd, err := f.dom(d)
	if err != nil {
		return golibvirt.DomainSnapshot{}, err
	}
	name := x[strings.Index(x, "<name>")+6 : strings.Index(x, "</name>")]
	if fd.snapshots == nil {
		fd.snapshots = map[string]bool{}
	}
	fd.snapshots[name] = true
	return golibvirt.DomainSnapshot{Name: name, Dom: d}, nil
}

func (f *fakeClient) DomainSnapshotLookupByName(d golibvirt.Domain, name string, _ uint32) (golibvirt.DomainSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fd, err := f.dom(d)
	if err != nil {
		return golibvirt.DomainSnapshot{}, err
	}
	if !fd.snapshots[name] {
		return golibvirt.DomainSnapshot{}, libvirtErr(golibvirt.ErrNoDomainSnapshot, "no snapshot")
	}
	return golibvirt.DomainSnapshot{Name: name, Dom: d}, nil
}

func (f *fakeClient) DomainRevertToSnapshot(s golibvirt.DomainSnapshot, flags uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("snapshot-revert %s %s %d", s.Dom.Name, s.Name, flags)
	return nil
}

func (f *fakeClient) DomainSnapshotDelete(s golibvirt.DomainSnapshot, flags golibvirt.DomainSnapshotDeleteFlags) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("snapshot-delete %s %s %d", s.Dom.Name, s.Name, flags)
	delete(f.doms[s.Dom.Name].snapshots, s.Name)
	return nil
}

func TestScreenshot(t *testing.T) {
	frame := bytes.Repeat([]byte{7}, 3<<20)
	c, _ := newFake(map[string]*fakeDomain{"brig-spike-a": {state: golibvirt.DomainRunning, owned: true, screen: frame}})
	got, mime, err := c.Screenshot("brig-spike-a")
	if err != nil || !bytes.Equal(got, frame) || mime != "image/x-portable-pixmap" {
		t.Errorf("Screenshot = %d bytes, %q, %v", len(got), mime, err)
	}
	if _, _, err := c.Screenshot("brig-spike-b"); err == nil {
		t.Error("Screenshot of a missing domain succeeded")
	}
}

func TestScreenshotTooLarge(t *testing.T) {
	c, _ := newFake(map[string]*fakeDomain{"brig-spike-a": {state: golibvirt.DomainRunning, owned: true, screen: make([]byte, MaxScreenshot+1)}})
	if _, _, err := c.Screenshot("brig-spike-a"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v, want a size error", err)
	}
}

func TestSendKeys(t *testing.T) {
	c, f := newFake(map[string]*fakeDomain{"brig-spike-a": {state: golibvirt.DomainRunning, owned: true}})
	if err := c.SendKeys("brig-spike-a", []uint32{29, 46}, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	checkCalls(t, f, "sendkey brig-spike-a 0 50 [29 46] 0")
	if err := c.SendKeys("brig-spike-a", nil, 0); err == nil {
		t.Error("SendKeys with no keys succeeded")
	}
	if err := c.SendKeys("brig-spike-a", make([]uint32, 17), 0); err == nil {
		t.Error("SendKeys with 17 keys succeeded")
	}
}

func TestAgentAndMonitorCommand(t *testing.T) {
	c, f := newFake(map[string]*fakeDomain{"brig-spike-a": {state: golibvirt.DomainRunning, owned: true}})
	f.reply = func(cmd string) (string, error) {
		if cmd == "fail" {
			return "", fmt.Errorf("agent not responding")
		}
		return `{"return":{}}`, nil
	}
	if out, err := c.AgentCommand("brig-spike-a", `{"execute":"guest-ping"}`, 0); err != nil || out != `{"return":{}}` {
		t.Errorf("AgentCommand = %q, %v", out, err)
	}
	if out, err := c.MonitorCommand("brig-spike-a", `{"execute":"query-status"}`); err != nil || out != `{"return":{}}` {
		t.Errorf("MonitorCommand = %q, %v", out, err)
	}
	if _, err := c.AgentCommand("brig-spike-a", "fail", 3*time.Second); err == nil {
		t.Error("failing agent command succeeded")
	}
	// The timeout is at least a second.
	checkCalls(t, f,
		`agent brig-spike-a {"execute":"guest-ping"} 1 0`,
		`monitor brig-spike-a {"execute":"query-status"} 0`,
		`agent brig-spike-a fail 3 0`)
}

func TestSnapshots(t *testing.T) {
	c, f := newFake(map[string]*fakeDomain{"brig-spike-a": {state: golibvirt.DomainRunning, owned: true}})
	if err := c.CreateSnapshot("brig-spike-a", "before-install"); err != nil {
		t.Fatal(err)
	}
	if err := c.RevertSnapshot("brig-spike-a", "before-install"); err != nil {
		t.Fatal(err)
	}
	if err := c.RevertSnapshot("brig-spike-a", "other"); err == nil {
		t.Error("revert to a missing snapshot succeeded")
	}
	if err := c.DeleteSnapshot("brig-spike-a", "before-install"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "a/b", "<x>", "-a", strings.Repeat("a", 65)} {
		if err := c.CreateSnapshot("brig-spike-a", bad); err == nil {
			t.Errorf("CreateSnapshot(%q) succeeded", bad)
		}
	}
	checkCalls(t, f,
		fmt.Sprintf(`snapshot-create brig-spike-a <domainsnapshot><name>before-install</name><memory snapshot="internal"/><disks><disk name="vda" snapshot="internal"/></disks></domainsnapshot> %d`, golibvirt.DomainSnapshotCreateAtomic),
		fmt.Sprintf("snapshot-revert brig-spike-a before-install %d", golibvirt.DomainSnapshotRevertForce),
		"snapshot-delete brig-spike-a before-install 0")
}

func TestSnapshotRefusesForeignDomain(t *testing.T) {
	c, _ := newFake(map[string]*fakeDomain{"other": {state: golibvirt.DomainRunning}})
	if err := c.CreateSnapshot("other", "s"); err == nil || !strings.Contains(err.Error(), "not created by brig") {
		t.Errorf("err = %v", err)
	}
}
