// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package action

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

func init() {
	register(
		Action{ID: "image-build", Build: buildImageBuild},
		Action{ID: "image-prune", Build: buildImagePrune},
		Action{ID: "image-push", Subject: subjectVM, Build: buildImagePush},
	)
}

var openShellVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func buildImageBuild(f Form, c Context) (Command, error) {
	argv := []string{"brig", "image", "build"}
	if s := strings.TrimSpace(f.Get("fedora")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 999 {
			return Command{}, errors.New("fedora: want a release number, such as 44")
		}
		if n != c.Snap.Config.Defaults.FedoraRelease {
			argv = append(argv, "--fedora", strconv.Itoa(n))
		}
	}
	if s := strings.TrimSpace(f.Get("openshell")); s != "" {
		if !openShellVersionRE.MatchString(s) {
			return Command{}, fmt.Errorf("OpenShell: want a version such as 0.1.2")
		}
		argv = append(argv, "--openshell", s)
	}
	return NewCommand(argv...).
		Note("Building takes a while and needs no root privileges. mkosi's output shows below as it comes."), nil
}

func buildImagePrune(_ Form, c Context) (Command, error) {
	cmd := NewCommand("brig", "image", "prune")
	var gone []string
	for i, img := range c.Snap.Images {
		if i > 0 && len(img.UsedBy) == 0 {
			gone = append(gone, img.ID)
		}
	}
	if len(gone) > 0 {
		return cmd.Note("Removes %s, and mkosi's package cache.", strings.Join(gone, ", ")), nil
	}
	return cmd.Note("No base image is unused. This only clears mkosi's package cache."), nil
}

func buildImagePush(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	if err := needRunning(v, "pushing an image"); err != nil {
		return Command{}, err
	}
	ref := strings.TrimSpace(f.Get("image"))
	switch {
	case ref == "":
		return Command{}, errors.New("image: name an image of your Podman, as podman images lists it")
	case len(ref) > 512 || strings.HasPrefix(ref, "-") || !plainText(ref) || strings.ContainsAny(ref, " \t\\"):
		return Command{}, fmt.Errorf("image: %q is no image name", ref)
	}
	return NewCommand("brig", "image", "push", v.Name, ref).
		Note("Copies the image from your Podman into %s with podman save and podman load, so sandboxes there can use it.", v.Name), nil
}
