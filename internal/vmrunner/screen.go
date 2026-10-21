// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmrunner

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"strconv"
)

// maxScreenSide bounds the width and height of a screenshot.
const maxScreenSide = 8192

// toPNG converts a screenshot in the format QEMU wrote it, PPM or PNG, to
// PNG and returns its size. The guest sets the display's size, so neither
// format is trusted.
func toPNG(data []byte, mime string) ([]byte, int, int, error) {
	if mime == "image/png" {
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, 0, 0, fmt.Errorf("screenshot: %w", err)
		}
		if cfg.Width > maxScreenSide || cfg.Height > maxScreenSide {
			return nil, 0, 0, fmt.Errorf("screenshot of %dx%d pixels is too large", cfg.Width, cfg.Height)
		}
		return data, cfg.Width, cfg.Height, nil
	}
	img, err := decodePPM(data)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("screenshot: %w", err)
	}
	var b bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&b, img); err != nil {
		return nil, 0, 0, err
	}
	return b.Bytes(), img.Rect.Dx(), img.Rect.Dy(), nil
}

// decodePPM decodes a binary PPM (P6) image with 8-bit samples, the format
// of QEMU's screendump.
func decodePPM(data []byte) (*image.RGBA, error) {
	rest, ok := bytes.CutPrefix(data, []byte("P6"))
	if !ok {
		return nil, errors.New("not a binary PPM image")
	}
	var fields [3]int
	for i := range fields {
		var tok []byte
		var err error
		if tok, rest, err = ppmToken(rest); err != nil {
			return nil, err
		}
		if fields[i], err = strconv.Atoi(string(tok)); err != nil || fields[i] < 1 {
			return nil, fmt.Errorf("invalid PPM header field %q", tok)
		}
	}
	w, h, maxval := fields[0], fields[1], fields[2]
	if w > maxScreenSide || h > maxScreenSide {
		return nil, fmt.Errorf("PPM image of %dx%d pixels is too large", w, h)
	}
	if maxval != 255 {
		return nil, fmt.Errorf("PPM image with maximum value %d, want 255", maxval)
	}
	// One whitespace byte ends the header.
	if len(rest) == 0 || !isSpace(rest[0]) {
		return nil, errors.New("PPM header not followed by whitespace")
	}
	rest = rest[1:]
	if len(rest) != w*h*3 {
		return nil, fmt.Errorf("PPM image of %dx%d pixels has %d bytes of data, want %d", w, h, len(rest), w*h*3)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i, j := 0, 0; i < len(rest); i, j = i+3, j+4 {
		img.Pix[j], img.Pix[j+1], img.Pix[j+2], img.Pix[j+3] = rest[i], rest[i+1], rest[i+2], 0xff
	}
	return img, nil
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// ppmToken returns the next token of a PPM header, skipping whitespace and
// comments, and what follows it.
func ppmToken(b []byte) ([]byte, []byte, error) {
	for len(b) > 0 {
		switch {
		case isSpace(b[0]):
			b = b[1:]
		case b[0] == '#':
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				return nil, nil, errors.New("truncated PPM header")
			}
			b = b[i+1:]
		default:
			i := 0
			for i < len(b) && b[i] >= '0' && b[i] <= '9' && i < 6 {
				i++
			}
			if i == 0 {
				return nil, nil, fmt.Errorf("invalid PPM header byte %q", b[0])
			}
			return b[:i], b[i:], nil
		}
	}
	return nil, nil, errors.New("truncated PPM header")
}
