// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package policy

// The annotations that brig sends with a change to a sandbox's policy. The
// gateway keeps them with the revision the change makes, as its provenance,
// and no later change alters them, so the audit trail can say who made a
// revision.
const (
	// AnnotationBy names the brig command that made the change.
	AnnotationBy = "brig.dev/by"
	// AnnotationRule, AnnotationHost, AnnotationPort and AnnotationBinary
	// describe the rule that the command added.
	AnnotationRule   = "brig.dev/rule"
	AnnotationHost   = "brig.dev/host"
	AnnotationPort   = "brig.dev/port"
	AnnotationBinary = "brig.dev/binary"
	// AnnotationTrace is the trace of the command's run in the activity
	// log, which brig ui hands on to the commands it runs.
	AnnotationTrace = "brig.dev/trace"
)

// ByPolicyAllow is AnnotationBy of brig policy allow.
const ByPolicyAllow = "policy-allow"
