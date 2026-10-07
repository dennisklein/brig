// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package bytesize

import "testing"

func TestParse(t *testing.T) {
	for in, want := range map[string]Size{
		"0":       0,
		"512":     512,
		"512MiB":  512 * MiB,
		"8G":      8 * GiB,
		"8gb":     8 * GiB,
		"20 GiB":  20 * GiB,
		"1.5GiB":  1536 * MiB,
		"2TiB":    2 * TiB,
		" 64KiB ": 64 * KiB,
	} {
		got, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Parse(%q) = %d, want %d", in, got, want)
		}
	}
	for _, in := range []string{"", "GiB", "-1G", "1.5", "8 parsecs", "1.2.3G", "1e3", "8388608TiB", "9223372036854775807"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", in)
		}
	}
}

func TestString(t *testing.T) {
	for s, want := range map[Size]string{
		0:          "0B",
		1023:       "1023B",
		KiB:        "1KiB",
		1536 * MiB: "1536MiB",
		8 * GiB:    "8GiB",
		3 * TiB:    "3TiB",
	} {
		if got := s.String(); got != want {
			t.Errorf("Size(%d).String() = %q, want %q", uint64(s), got, want)
		}
	}
}

func TestTextRoundTrip(t *testing.T) {
	var s Size
	if err := s.UnmarshalText([]byte("40GiB")); err != nil {
		t.Fatal(err)
	}
	b, err := s.MarshalText()
	if err != nil || string(b) != "40GiB" {
		t.Fatalf("MarshalText() = %q, %v", b, err)
	}
}
