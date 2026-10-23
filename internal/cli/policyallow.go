// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/policy"
	"github.com/dennisklein/brig/internal/vm"
)

// allowRequest is what brig policy allow is asked to do.
type allowRequest struct {
	workspace, sandbox string
	// draft is the chunk ID of OpenShell's draft to approve. Otherwise the
	// rule is described by host, port, binary, protocol, access and
	// enforcement.
	draft       string
	host        string
	port        int
	binary      string
	protocol    string // "rest", or "" for the whole connection
	access      string
	enforcement string
	rule        string // the rule's key; a draft's with save
	broad       bool   // the user confirmed a broad rule
	save        bool
	to          string
}

func newPolicyAllowCmd() *cobra.Command {
	var (
		req    allowRequest
		l4     bool
		audit  bool
		broad  bool
		access string
	)
	cmd := &cobra.Command{
		Use:   "allow [VM] SANDBOX HOST[:PORT] --binary PATH [--access LEVEL] [--save [--to FILE]]\n  brig policy allow [VM] SANDBOX --draft CHUNK-ID [--rule KEY] [--save [--to FILE]]",
		Short: "Let a sandbox's program reach a host, and keep the rule for new sandboxes",
		Long: `Add one network rule to the policy of SANDBOX, as openshell policy update
does, and wait until the sandbox loads it. The rule lets the program --binary
reach HOST on PORT (443 if not given) and no other sandbox gets it.

brig sends the rule to the gateway itself, with annotations that OpenShell
keeps with the policy revision: brig.dev/by=policy-allow, the rule, its host,
port and program, and brig.dev/trace, the run's trace in the activity log.
brig audit shows them as who made the change.

By default the rule inspects HTTP requests and allows the methods that
--access names: read-only (GET, HEAD, OPTIONS), read-write (also POST, PUT,
PATCH) or full. --l4 allows the whole connection instead, without looking at
the requests, and --audit only logs what the access level would deny. The rule
is named allow_HOST_PORT, such as allow_registry_npmjs_org_443, with ANY for a
wildcard (allow_ANY_example_com_443 for *.example.com), or --rule says how.

brig refuses a wildcard that stands directly above a top-level domain or
another public suffix, such as *.com, *.co.uk or *.github.io, and * or **. It
asks for --broad before it adds a rule with full access, with --l4, or for a
program path with a wildcard.

The gateway adds the endpoint and the program to a rule of the same name that
the sandbox has already, and in a rule every program reaches every endpoint.
So brig refuses to add to a rule named allow_HOST_PORT that holds other hosts
or ports, and asks for --broad before the program gets the whole connection or
full access of the rule's endpoints, or before programs of the rule with a
wildcard reach the new endpoint.

With --draft, brig approves the rule that OpenShell drafted from the denials
of the sandbox (brig ui shows them) as it is, with
openshell rule approve. The draft's rule is then the one that --save saves,
so --rule names it.

--save then writes the rule into the sandbox's policy file, so that sandboxes
created from it get the rule too, as brig policy save does: it shows the
change to the file first and does not ask. brig checks that it can save
before it changes the sandbox. A sandbox created from the default policy
needs --to NAME, which creates policies/NAME.yaml.

Without VM, the one that brig use selected is used.`,
		Example: `  brig policy allow dev webapp registry.npmjs.org --binary /usr/bin/node
  brig policy allow dev webapp api.example.com:8443 --binary /usr/bin/curl --access read-write --save --to webapp
  brig policy allow dev webapp --draft ch-17 --rule allow_registry_npmjs_org_443 --save --to webapp`,
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("draft") {
				return cobra.RangeArgs(1, 2)(cmd, args)
			}
			return cobra.RangeArgs(2, 3)(cmd, args)
		},
		ValidArgsFunction: completeVMAndSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			fl := cmd.Flags()
			if req.draft != "" {
				for _, f := range []string{"binary", "access", "l4", "audit", "broad"} {
					if fl.Changed(f) {
						return fmt.Errorf("--%s describes a rule of your own, and --draft approves the draft as OpenShell wrote it", f)
					}
				}
				req.sandbox = args[len(args)-1]
				args = args[:len(args)-1]
				switch {
				case req.save && req.rule == "":
					return errors.New("--save with --draft needs --rule, the key of the rule that the draft adds to")
				case req.rule != "" && !ruleKeyRE.MatchString(req.rule):
					return errors.New("--rule: use letters, digits, _, . and - only")
				}
			} else {
				req.sandbox = args[len(args)-2]
				host, err := parseHostPort(args[len(args)-1])
				if err != nil {
					return err
				}
				req.host, req.port = host.name, host.port
				args = args[:len(args)-2]
				if err := checkAllowFlags(&req, fl.Changed("access"), access, l4, audit, broad); err != nil {
					return err
				}
			}
			if req.to != "" && !req.save {
				return errors.New("--to needs --save")
			}
			var err error
			if req.workspace, err = chosenWorkspace(req.workspace); err != nil {
				return err
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			v, err := a.resolveVMArg(cmd, args)
			if err != nil {
				return err
			}
			return a.policyAllow(cmd.Context(), v, req, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.draft, "draft", "", "approve OpenShell's draft with this chunk ID, as it is")
	f.StringVar(&req.binary, "binary", "", "the full path of the program that may connect")
	f.StringVar(&access, "access", "read-only", "the HTTP methods to allow: read-only, read-write or full")
	f.BoolVar(&l4, "l4", false, "allow the whole connection, without inspecting HTTP requests")
	f.BoolVar(&audit, "audit", false, "only log what the access level would deny, and let it pass")
	f.BoolVar(&broad, "broad", false, "confirm a rule with full access, with --l4, or for a program path with a wildcard")
	f.StringVar(&req.rule, "rule", "", "the key of the rule (default allow_HOST_PORT); with --draft, the draft's rule")
	f.BoolVar(&req.save, "save", false, "write the rule into the sandbox's policy file, as brig policy save does")
	f.StringVar(&req.to, "to", "", "with --save: save into this policy file, or create policies/NAME.yaml from the sandbox's policy file")
	addWorkspaceFlag(cmd, &req.workspace)
	_ = cmd.RegisterFlagCompletionFunc("to", completePolicyFiles)
	return cmd
}

type hostPort struct {
	name string
	port int
}

// parseHostPort parses HOST or HOST:PORT; the port is 443 if not given.
func parseHostPort(s string) (hostPort, error) {
	host, port := strings.ToLower(s), 443
	if h, p, ok := strings.Cut(host, ":"); ok {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return hostPort{}, fmt.Errorf("port %q: write a number from 1 to 65535", p)
		}
		host, port = h, n
	}
	if err := policy.CheckHost(host); err != nil {
		return hostPort{}, fmt.Errorf("host %q: %w", host, err)
	}
	return hostPort{host, port}, nil
}

// checkAllowFlags fills the rule of req from the flags of a rule of the
// user's own, and refuses the ones that do not fit.
func checkAllowFlags(req *allowRequest, accessGiven bool, access string, l4, audit, broad bool) error {
	switch {
	case req.binary == "":
		return errors.New("say which program may connect with --binary, by its full path")
	case !path.IsAbs(req.binary) || strings.ContainsFunc(req.binary, func(r rune) bool { return r < ' ' || r == 0x7f }):
		return errors.New("--binary: write the program's full path, such as /usr/bin/node")
	}
	if req.rule == "" {
		req.rule = policy.RuleName(req.host, req.port)
	}
	if !ruleKeyRE.MatchString(req.rule) {
		return errors.New("--rule: use letters, digits, _, . and - only")
	}
	if strings.HasPrefix(req.rule, "_provider_") {
		return errors.New("--rule: names starting with _provider_ belong to the rules of providers")
	}
	switch {
	case l4:
		if accessGiven || audit {
			return errors.New("--l4 allows the whole connection; --access and --audit apply to HTTP requests")
		}
		req.protocol = ""
	default:
		req.protocol = "rest"
		switch access {
		case "read-only", "read-write", "full":
		default:
			return fmt.Errorf("--access: choose read-only, read-write or full, not %q", access)
		}
		req.access, req.enforcement = access, "enforce"
		if audit {
			req.enforcement = "audit"
		}
	}
	req.broad = broad
	if why := policy.Broad(req.protocol, req.access, req.binary); len(why) > 0 && !broad {
		return fmt.Errorf("this rule is broad: %s; add --broad to add it anyway", strings.Join(why, "; "))
	}
	return nil
}

// policyLoadTimeout bounds the wait for a sandbox to load a new policy
// revision, as openshell policy update --wait waits.
const policyLoadTimeout = time.Minute

// addRule adds a network rule to a sandbox through the gateway's gRPC API,
// with the annotations, and waits until the sandbox loads it, saying so on
// out; tests replace it.
var addRule = func(ctx context.Context, v *vm.VM, workspace, sandbox string, rule osclient.NetworkRule, annotations map[string]string, out io.Writer) error {
	c, err := dialGateway(ctx, v)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	ch, err := c.AddRule(ctx, workspace, sandbox, rule, annotations)
	if err != nil {
		return fmt.Errorf("adding rule %s to sandbox %s: %s", rule.Key, sandbox, callMessage(err))
	}
	if ch.Unchanged {
		fmt.Fprintf(out, "Sandbox %s has the rule already; its policy stays at version %d.\n", termtext.Escape(sandbox), ch.Version)
		return nil
	}
	wctx, cancel := context.WithTimeout(ctx, policyLoadTimeout)
	defer cancel()
	how, active, err := c.WaitLoaded(wctx, workspace, sandbox, ch.Version)
	if err != nil {
		return fmt.Errorf("policy version %d of sandbox %s: %s", ch.Version, sandbox, callMessage(err))
	}
	if how == osclient.PolicySuperseded {
		fmt.Fprintf(out, "Policy version %d of %s was superseded by a newer change; the sandbox enforces version %d.\n", ch.Version, termtext.Escape(sandbox), active)
		return nil
	}
	fmt.Fprintf(out, "Sandbox %s loaded policy version %d with rule %s.\n", termtext.Escape(sandbox), ch.Version, termtext.Escape(rule.Key))
	return nil
}

// callMessage is what a gateway call's error says, without gRPC's framing.
func callMessage(err error) string {
	if s, ok := status.FromError(err); ok && s.Code() != codes.OK {
		return s.Message()
	}
	return err.Error()
}

// allowAnnotations are the annotations of the revision that brig policy
// allow makes: they name the command, the rule and the run's trace in the
// activity log, so that the audit trail can tell who allowed what.
func allowAnnotations(ctx context.Context, req allowRequest) map[string]string {
	a := map[string]string{
		policy.AnnotationBy:     policy.ByPolicyAllow,
		policy.AnnotationRule:   req.rule,
		policy.AnnotationHost:   req.host,
		policy.AnnotationPort:   strconv.Itoa(req.port),
		policy.AnnotationBinary: req.binary,
	}
	if b := progress.BusFrom(ctx); b != nil {
		a[policy.AnnotationTrace] = b.TraceID().String()
	}
	return a
}

// checkMerge refuses to add the rule of req to the sandbox whose base
// policy is base where the gateway would merge it into a rule of the same
// key that grants more than req asks for. In a rule, every program reaches
// every endpoint, so a rule named after the host of req must hold no other
// endpoints, which the program would reach too, and a merge that lets a
// program reach more broadly needs --broad, as a broad rule of its own does.
// The rule may still change between this check and the change.
func checkMerge(base []byte, req allowRequest) error {
	doc, err := policy.Parse(base)
	if err != nil {
		return fmt.Errorf("sandbox %s: %w", req.sandbox, err)
	}
	rule, ok := doc.Network[req.rule]
	if !ok {
		return nil
	}
	if req.rule == policy.RuleName(req.host, req.port) {
		if other := policy.OtherEndpoints(rule, req.host, req.port); len(other) > 0 {
			return fmt.Errorf("sandbox %s has a rule %s for %s already, which %s would reach too; name a rule of its own with --rule",
				req.sandbox, req.rule, strings.Join(other, ", "), req.binary)
		}
	}
	if why := policy.MergeBroad(rule, req.host, req.port, req.binary); len(why) > 0 && !req.broad {
		return fmt.Errorf("sandbox %s has a rule %s already, and adding to it is broad: %s; add --broad to add it anyway, or name a rule of its own with --rule",
			req.sandbox, req.rule, strings.Join(why, "; "))
	}
	return nil
}

// ruleKeyRE matches a rule's key under network_policies.
var ruleKeyRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// policyAllow adds the rule to the sandbox and, with req.save, saves it.
func (a *app) policyAllow(ctx context.Context, v *vm.VM, req allowRequest, out, warn io.Writer) error {
	// Refuse a save that cannot be made before the sandbox changes.
	var base []byte
	if req.save {
		t, err := a.findSaveTarget(ctx, v, req.workspace, req.sandbox, req.to, warn)
		if err != nil {
			return err
		}
		base = t.rep.base
	}
	cli, err := openshell.Find()
	if err != nil {
		return err
	}
	if req.draft == "" {
		if base == nil {
			if base, err = cli.BasePolicy(ctx, v.GatewayName(), req.workspace, req.sandbox); err != nil {
				return fmt.Errorf("the base policy of sandbox %s in workspace %s of VM %s (is the VM running?): %w", req.sandbox, req.workspace, v.Name, err)
			}
		}
		if err := checkMerge(base, req); err != nil {
			return err
		}
	}
	if req.draft != "" {
		text, err := cli.InSandbox(ctx, v.GatewayName(), req.workspace, req.sandbox, "rule", "approve", req.sandbox, "--chunk-id", req.draft)
		if err != nil {
			return err
		}
		if len(text) > 0 {
			_, _ = io.WriteString(out, termtext.EscapeLines(string(text)))
		}
	} else {
		rule := osclient.NetworkRule{
			Key: req.rule, Host: req.host, Port: req.port, Protocol: req.protocol,
			Access: req.access, Enforcement: req.enforcement, Binaries: []string{req.binary},
		}
		if err := addRule(ctx, v, req.workspace, req.sandbox, rule, allowAnnotations(ctx, req), out); err != nil {
			return err
		}
	}
	if req.protocol == "" && req.draft == "" {
		fmt.Fprintln(out, "The whole connection is allowed: the proxy does not look at the HTTP requests in it.")
	}
	if !req.save {
		keep := "brig policy save " + v.Name + " " + req.sandbox
		if req.rule != "" {
			keep += " --rule " + req.rule
		}
		fmt.Fprintf(out, "Only %s has the rule. %s keeps it for new sandboxes.\n", termtext.Escape(req.sandbox), termtext.Escape(keep))
		return nil
	}
	err = a.policySave(ctx, v, saveRequest{
		workspace: req.workspace, sandbox: req.sandbox, keys: []string{req.rule}, to: req.to, show: true,
	}, out, warn)
	var same ruleUnchangedError
	switch {
	case errors.As(err, &same):
		fmt.Fprintf(out, "Nothing to save: %s has rule %s already.\n", termtext.Escape(same.file), termtext.Escape(req.rule))
		return nil
	case err != nil:
		return fmt.Errorf("sandbox %s has the rule now, but saving it failed (brig policy save %s %s --rule %s retries): %w", req.sandbox, v.Name, req.sandbox, req.rule, err)
	}
	return nil
}
