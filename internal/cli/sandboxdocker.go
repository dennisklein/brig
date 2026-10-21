// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	_ "embed"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/policy"
	"github.com/dennisklein/brig/internal/vm"
)

const (
	// dockerRuleKey is the network rule that brig sandbox create --docker
	// adds to the sandbox's policy.
	dockerRuleKey = "brig_docker"
	// dockerLabel marks the sandboxes of brig sandbox create --docker.
	dockerLabel = "brig.dev/docker"
	// dockerHost is how a sandbox names the VM's loopback, where Docker
	// listens: OpenShell maps it to the VM's 127.0.0.1.
	dockerHost = "host.openshell.internal"
)

// dockerBinaries are the programs in a sandbox that may call Docker: the
// docker CLI and its plugins, and the runtimes in which testcontainers talks
// to Docker's API itself. OpenShell matches the path of the running
// executable, after symbolic links, with * and ** globs. Go builds test
// binaries below /tmp/go-build*.
//
// The list keeps other programs, such as curl, off Docker, but each runtime
// in it can drive Docker in full: the opt-in is the boundary, not the list.
var dockerBinaries = []string{
	"/usr/bin/docker",
	"/usr/local/bin/docker",
	"/usr/bin/docker-compose",
	"/usr/local/bin/docker-compose",
	"/usr/libexec/docker/cli-plugins/*",
	"/usr/lib/docker/cli-plugins/*",
	"/usr/local/libexec/docker/cli-plugins/*",
	"/usr/local/lib/docker/cli-plugins/*",
	"/usr/bin/python3*",
	"/usr/local/bin/python3*",
	"/usr/lib/jvm/**/bin/java",
	"/opt/java/**/bin/java",
	"/usr/bin/node",
	"/usr/local/bin/node",
	"/tmp/go-build*/**",
}

// dockerPorts are the VM's ports that a sandbox with Docker may reach: the
// Docker API and the ports that Docker publishes containers at.
func dockerPorts() []int {
	ports := []int{guest.DockerPort}
	for p := guest.FirstPublishedPort; p <= guest.LastPublishedPort; p++ {
		ports = append(ports, p)
	}
	return ports
}

// dockerRule returns the network rule that lets a sandbox use the VM's
// Docker. The connections are plain TCP, which OpenShell does not inspect.
func dockerRule() policy.NamedRule {
	var ports, bins []any
	for _, p := range dockerPorts() {
		ports = append(ports, p)
	}
	for _, b := range dockerBinaries {
		bins = append(bins, map[string]any{"path": b})
	}
	return policy.NamedRule{Key: dockerRuleKey, Rule: map[string]any{
		"name":      dockerRuleKey,
		"endpoints": []any{map[string]any{"host": dockerHost, "ports": ports, "protocol": "tcp"}},
		"binaries":  bins,
	}}
}

// dockerEnv is the environment of a sandbox with Docker: the docker CLI and
// testcontainers find the daemon, testcontainers reaches the published ports
// through OpenShell, and its Ryuk container mounts dockerd's socket.
func dockerEnv() []string {
	return []string{
		"DOCKER_HOST=tcp://" + dockerHost + ":" + strconv.Itoa(guest.DockerPort),
		"TESTCONTAINERS_HOST_OVERRIDE=" + dockerHost,
		"TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=" + guest.DockerSocket,
	}
}

// builtinPolicy is the policy that OpenShell gives a sandbox when nothing
// names one: openshell_policy::restrictive_default_policy.
//
//go:embed sandboxdocker_default.yaml
var builtinPolicy []byte

// dockerPolicy returns the policy of a new sandbox with Docker: the policy
// file that the openshell options args name with --policy, else VM v's
// default policy, else OpenShell's built-in default, with dockerRule unless
// it has a rule of that key already, which is the user's own. It also
// returns args without --policy.
func dockerPolicy(v *vm.VM, args []string) (data []byte, rest []string, err error) {
	file, rest, err := cutPolicyArg(args)
	if err != nil {
		return nil, nil, err
	}
	if file == "" {
		if file, err = defaultPolicy(v); err != nil {
			return nil, nil, err
		}
	}
	base := builtinPolicy
	if file != "" {
		if base, err = os.ReadFile(file); err != nil {
			return nil, nil, err
		}
	}
	doc, err := policy.Parse(base)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", file, err)
	}
	if _, ok := doc.Network[dockerRuleKey]; ok {
		return base, rest, nil
	}
	data, err = policy.Save(base, []policy.NamedRule{dockerRule()})
	if err != nil {
		if file == "" {
			file = "OpenShell's built-in default policy"
		}
		return nil, nil, fmt.Errorf("adding the Docker rule to %s: %w", file, err)
	}
	return data, rest, nil
}

// cutPolicyArg returns the file that the openshell options in args name
// with --policy, and args without it. Options end at --.
func cutPolicyArg(args []string) (file string, rest []string, err error) {
	opts := sandboxOptions(args)
	rest = slices.Clone(args[len(opts):])
	var kept []string
	for i := 0; i < len(opts); i++ {
		arg := opts[i]
		if v, ok := strings.CutPrefix(arg, "--policy="); ok {
			file = v
			continue
		}
		if arg == "--policy" {
			if i+1 == len(opts) {
				return "", nil, fmt.Errorf("--policy needs a file")
			}
			file = opts[i+1]
			i++
			continue
		}
		kept = append(kept, arg)
	}
	return file, append(kept, rest...), nil
}

// memFile returns a path at which the program that brig runs or becomes
// reads data, without writing it to disk: a memfd that is not closed on
// exec, as /proc/self/fd/N. OpenShell reads the file in full when it
// starts, and the memfd goes with the last process that holds it.
var memFile = func(name string, data []byte) (string, error) {
	fd, err := unix.MemfdCreate(name, 0)
	if err != nil {
		return "", fmt.Errorf("memfd for %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("memfd for %s: %w", name, err)
	}
	// f stays open: closing it would close fd.
	keepOpen = append(keepOpen, f)
	return "/proc/self/fd/" + strconv.Itoa(fd), nil
}

// keepOpen holds the files of memFile, so that the garbage collector does
// not close them.
var keepOpen []*os.File

// dockerArgs returns the openshell sandbox create options that give the
// sandbox in VM v Docker, for the openshell options args, and args without
// the --policy that the policy replaces. A sandbox from a template gets no
// environment, since openshell takes no --env with --template; brig says so
// on w.
func dockerArgs(v *vm.VM, args []string, w io.Writer) (opts, rest []string, err error) {
	data, rest, err := dockerPolicy(v, args)
	if err != nil {
		return nil, nil, err
	}
	path, err := memFile("brig-docker-policy.yaml", data)
	if err != nil {
		return nil, nil, err
	}
	opts = []string{"--policy", path, "--label", dockerLabel + "=true"}
	if !hasTemplateArg(rest) {
		for _, e := range dockerEnv() {
			opts = append(opts, "--env", e)
		}
		return opts, rest, nil
	}
	fmt.Fprintf(w, "The sandbox may use Docker, but openshell sets no environment with --template. In the sandbox, set:\n")
	for _, e := range dockerEnv() {
		fmt.Fprintf(w, "  export %s\n", e)
	}
	return opts, rest, nil
}

// hasTemplateArg reports whether the openshell options in args include
// --template.
func hasTemplateArg(args []string) bool {
	return slices.ContainsFunc(sandboxOptions(args), func(a string) bool {
		return a == "--template" || strings.HasPrefix(a, "--template=")
	})
}

// withoutDockerRule returns the policy of a sandbox without the rule of
// brig sandbox create --docker, unless file has a rule of that key too: brig
// added it, not the user, so it does not differ from the file.
func withoutDockerRule(sandbox, file *policy.Doc) *policy.Doc {
	if _, ok := sandbox.Network[dockerRuleKey]; !ok {
		return sandbox
	}
	if file != nil {
		if _, ok := file.Network[dockerRuleKey]; ok {
			return sandbox
		}
	}
	d := *sandbox
	d.Network = maps.Clone(sandbox.Network)
	delete(d.Network, dockerRuleKey)
	return &d
}
