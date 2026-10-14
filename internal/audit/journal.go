// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// The journal mirror's modes, as audit.journal in config.yaml sets them.
const (
	// JournalChanges mirrors denials, drops, policy and draft changes,
	// sandboxes and gaps: what Record.Change reports.
	JournalChanges = "changes"
	// JournalAll mirrors allowed requests too, which the journal's rate
	// limit may drop.
	JournalAll = "all"
	JournalOff = "off"
)

// JournalSocket is where journald takes entries in its native protocol.
const JournalSocket = "/run/systemd/journal/socket"

// syslog priorities.
const (
	prioWarning = 4
	prioNotice  = 5
	prioInfo    = 6
)

// Journal mirrors records to the journal. Losing an entry costs nothing:
// the audit trail's files are the record.
type Journal struct {
	// All mirrors allowed requests too.
	All  bool
	conn *net.UnixConn
}

// OpenJournal connects to journald's socket at path.
func OpenJournal(path string, all bool) (*Journal, error) {
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, fmt.Errorf("connecting to the journal: %w", err)
	}
	return &Journal{All: all, conn: conn}, nil
}

// Send mirrors the record, if the mirror takes its kind.
func (j *Journal) Send(r Record) error {
	if j == nil || (!j.All && !r.Change()) {
		return nil
	}
	_, err := j.conn.Write(JournalEntry(r))
	return err
}

// Close disconnects from the journal.
func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	return j.conn.Close()
}

// maxMessage bounds an entry's message.
const maxMessage = 4096

// JournalEntry is the record as an entry of journald's native protocol.
func JournalEntry(r Record) []byte {
	var b bytes.Buffer
	field := func(k, v string) {
		if v == "" {
			return
		}
		if !strings.ContainsRune(v, '\n') {
			b.WriteString(k + "=" + v + "\n")
			return
		}
		b.WriteString(k + "\n")
		_ = binary.Write(&b, binary.LittleEndian, uint64(len(v)))
		b.WriteString(v + "\n")
	}
	msg := r.VM
	if k := r.SandboxKey(); k != "" {
		msg += " " + k
	}
	msg += ": " + Describe(r)
	if len(msg) > maxMessage {
		msg = msg[:maxMessage]
	}
	prio := prioNotice
	switch {
	case r.Denied(), r.Kind == KindGap, r.Kind == KindPolicy && r.Status == "failed":
		prio = prioWarning
	case r.Kind == KindRequest:
		prio = prioInfo
	}
	field("MESSAGE", msg)
	field("PRIORITY", strconv.Itoa(prio))
	field("SYSLOG_IDENTIFIER", "brig-audit")
	field("BRIG_VM", r.VM)
	field("BRIG_WORKSPACE", r.Workspace)
	field("BRIG_SANDBOX", r.Sandbox)
	field("BRIG_KIND", r.Kind)
	field("BRIG_VERDICT", r.Verdict)
	field("BRIG_HOST", r.Host)
	if r.Port != 0 {
		field("BRIG_PORT", strconv.Itoa(r.Port))
	}
	if r.Kind == KindDrop {
		field("BRIG_PROTO", r.Proto)
		field("BRIG_REASON", r.Reason)
		field("BRIG_HITS", strconv.Itoa(r.Hits))
	}
	field("BRIG_BINARY", r.Binary)
	field("BRIG_RULE", r.Rule)
	if r.Kind == KindPolicy {
		field("BRIG_POLICY_VERSION", strconv.FormatUint(uint64(r.Version), 10))
	}
	field("BRIG_BY", r.By)
	field("BRIG_CHUNK", r.Chunk)
	field("BRIG_TRACE", r.Trace)
	return b.Bytes()
}
