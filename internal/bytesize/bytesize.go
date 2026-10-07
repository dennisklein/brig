// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package bytesize parses and formats byte quantities such as "8GiB".
package bytesize

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Size is a number of bytes.
type Size uint64

// Binary units.
const (
	B   Size = 1
	KiB      = 1024 * B
	MiB      = 1024 * KiB
	GiB      = 1024 * MiB
	TiB      = 1024 * GiB
)

var units = map[string]Size{
	"": B, "B": B,
	"K": KiB, "KB": KiB, "KIB": KiB,
	"M": MiB, "MB": MiB, "MIB": MiB,
	"G": GiB, "GB": GiB, "GIB": GiB,
	"T": TiB, "TB": TiB, "TIB": TiB,
}

// Parse reads a size such as "512MiB", "8G" or "20 GiB". Units are binary
// (powers of 1024) whether or not they carry the "i"; a bare number is bytes.
func Parse(s string) (Size, error) {
	t := strings.TrimSpace(s)
	i := strings.IndexFunc(t, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	num, unit := t, ""
	if i >= 0 {
		num, unit = t[:i], strings.ToUpper(strings.TrimSpace(t[i:]))
	}
	mult, ok := units[unit]
	if num == "" || !ok {
		return 0, fmt.Errorf("invalid size %q: use a number with an optional unit such as MiB or GiB", s)
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	b := v * float64(mult)
	// float64(math.MaxInt64) is 2^63, one more than int64 holds.
	if b >= math.MaxInt64 {
		return 0, fmt.Errorf("invalid size %q: too large", s)
	}
	if b != math.Trunc(b) {
		return 0, fmt.Errorf("invalid size %q: not a whole number of bytes", s)
	}
	return Size(b), nil
}

// String formats the size with the largest binary unit that divides it.
func (s Size) String() string {
	for _, u := range []struct {
		name string
		size Size
	}{{"TiB", TiB}, {"GiB", GiB}, {"MiB", MiB}, {"KiB", KiB}} {
		if s >= u.size && s%u.size == 0 {
			return strconv.FormatUint(uint64(s/u.size), 10) + u.name
		}
	}
	return strconv.FormatUint(uint64(s), 10) + "B"
}

// MiB returns the size in whole mebibytes, rounding down.
func (s Size) MiB() uint64 { return uint64(s / MiB) }

// MarshalText implements encoding.TextMarshaler.
func (s Size) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Size) UnmarshalText(text []byte) error {
	v, err := Parse(string(text))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// Set implements pflag.Value.
func (s *Size) Set(v string) error { return s.UnmarshalText([]byte(v)) }

// Type implements pflag.Value.
func (s *Size) Type() string { return "size" }
