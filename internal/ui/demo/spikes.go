// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package demo

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"time"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/ui/store"
)

// Spikes are the made-up VMs that agents started through the vm runner.
func Spikes(now time.Time) []store.Spike {
	const gib = bytesize.Size(1 << 30)
	return []store.Spike{
		{
			Service: "vms", ID: "3f9c1a2e", Sandbox: "dev/default/webapp", Image: "fedora-44-desktop", State: "running",
			CPUs: 2, Memory: 4 * gib, Disk: 20 * gib, CreatedAt: now.Add(-38 * time.Minute), ExpiresAt: now.Add(3*time.Hour + 22*time.Minute),
			Snapshots: []string{"installed"},
		},
		{
			Service: "vms", ID: "b04d77c1", Sandbox: "dev/default/webapp", Image: "ubuntu-24.04", State: "shut off",
			CPUs: 1, Memory: 2 * gib, Disk: 10 * gib, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(2 * time.Hour),
		},
	}
}

// SpikeScreen draws a made-up desktop: a window on a gradient over a panel.
func SpikeScreen(_ context.Context, _, id string) ([]byte, error) {
	if id != "3f9c1a2e" {
		return nil, errors.New("the VM is shut off")
	}
	const w, h = 640, 400
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{uint8(30 + 40*y/h), uint8(60 + 60*x/w), 120, 255}
			switch {
			case y < 24:
				c = color.RGBA{24, 24, 28, 255}
			case x > 120 && x < 520 && y > 80 && y < 330:
				c = color.RGBA{246, 246, 244, 255}
				if y < 108 {
					c = color.RGBA{222, 222, 218, 255}
				}
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
