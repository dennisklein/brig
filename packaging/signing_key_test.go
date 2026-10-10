// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH stands in for the gh CLI. It logs each call to $FAKE_LOG and serves
// the deployment policies listed in $FAKE_POLICIES (tab-separated id, type,
// name). The environment has no key yet. $FAKE_FAIL names the call that fails: "list" or "delete".
const fakeGH = `#!/bin/sh
echo "$*" >> "$FAKE_LOG"
case "$*" in
  "auth status") ;;
  *"/secrets/"*) echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
  *"--paginate"*)
    if [ "$FAKE_FAIL" = list ]; then echo "gh: HTTP 502" >&2; exit 1; fi
    cat "$FAKE_POLICIES" ;;
  *"--method DELETE"*)
    if [ "$FAKE_FAIL" = delete ]; then echo "gh: HTTP 500" >&2; exit 1; fi ;;
  "secret set"*) cat > /dev/null ;;
esac
`

const fakeGPG = `#!/bin/sh
case " $* " in
  *" --quick-gen-key "*)
    mkdir -p "$GNUPGHOME/openpgp-revocs.d"
    : > "$GNUPGHOME/openpgp-revocs.d/FPR.rev" ;;
  *" --list-secret-keys"*) echo "fpr:::::::::FPR:" ;;
  *) echo key ;;
esac
`

// TestSigningKeyPolicies runs scripts/signing-key.sh against a fake gh, gpg
// and git, and checks which deployment policies of the rpm-signing
// environment it deletes and adds, and that the key is only stored once the
// environment is restricted to main.
func TestSigningKeyPolicies(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	script, err := filepath.Abs("../scripts/signing-key.sh")
	if err != nil {
		t.Fatal(err)
	}

	const (
		del  = "api --silent --method DELETE repos/dennisklein/brig/environments/rpm-signing/deployment-branch-policies/"
		post = "api --silent --method POST repos/dennisklein/brig/environments/rpm-signing/deployment-branch-policies -f name=main -f type=branch"
	)
	tests := []struct {
		name     string
		policies string
		fail     string
		wantOK   bool
		want     []string // call prefixes that must have been logged
		unwanted []string // call prefixes that must not have been logged
	}{
		{
			name:     "no policies",
			wantOK:   true,
			want:     []string{post, "secret set"},
			unwanted: []string{del},
		},
		{
			name:     "only main",
			policies: "7\tbranch\tmain\n",
			wantOK:   true,
			want:     []string{"secret set"},
			unwanted: []string{del, post},
		},
		{
			name:     "stale branch and tag policies are removed",
			policies: "7\tbranch\tmain\n8\tbranch\t*\n9\ttag\tv*\n",
			wantOK:   true,
			want:     []string{del + "8", del + "9", "secret set"},
			unwanted: []string{del + "7", post},
		},
		{
			name:     "main as a tag policy does not count",
			policies: "8\ttag\tmain\n",
			wantOK:   true,
			want:     []string{del + "8", post, "secret set"},
		},
		{
			name:     "failed listing aborts",
			policies: "8\tbranch\t*\n",
			fail:     "list",
			unwanted: []string{del, post, "secret set"},
		},
		{
			name:     "failed delete aborts",
			policies: "8\tbranch\t*\n",
			fail:     "delete",
			unwanted: []string{post, "secret set"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			root := filepath.Join(dir, "repo")
			for _, d := range []string{bin, filepath.Join(root, "packaging/brig-release")} {
				if err := os.MkdirAll(d, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			for name, body := range map[string]string{
				"gh":  fakeGH,
				"gpg": fakeGPG,
				"git": "#!/bin/sh\necho '" + root + "'\n",
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil { //nolint:gosec // G306: the scripts must be executable
					t.Fatal(err)
				}
			}
			policies := filepath.Join(dir, "policies")
			if err := os.WriteFile(policies, []byte(tt.policies), 0o600); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "log")

			cmd := exec.Command(bash, script)
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"HOME="+dir,
				"XDG_DATA_HOME="+filepath.Join(dir, "data"),
				"FAKE_LOG="+log,
				"FAKE_POLICIES="+policies,
				"FAKE_FAIL="+tt.fail,
			)
			out, err := cmd.CombinedOutput()
			if tt.wantOK && err != nil {
				t.Fatalf("script failed: %v\n%s", err, out)
			}
			if !tt.wantOK && err == nil {
				t.Fatalf("script succeeded, want failure\n%s", out)
			}

			data, _ := os.ReadFile(log)
			calls := strings.Split(strings.TrimSpace(string(data)), "\n")
			for _, w := range tt.want {
				if !hasCall(calls, w) {
					t.Errorf("call %q missing from:\n%s", w, data)
				}
			}
			for _, u := range tt.unwanted {
				if hasCall(calls, u) {
					t.Errorf("unexpected call %q in:\n%s", u, data)
				}
			}
			_, statErr := os.Stat(filepath.Join(root, "packaging/brig-release/RPM-GPG-KEY-brig"))
			if tt.wantOK == os.IsNotExist(statErr) {
				t.Errorf("public key written = %v, want %v", statErr == nil, tt.wantOK)
			}
		})
	}
}

// hasCall reports whether a logged call starts with prefix.
func hasCall(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// TestSigningKeyRotation runs scripts/signing-key.sh --next, --switch and
// --retire with the real gpg against a fake gh and git.
func TestSigningKeyRotation(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found")
	}
	script, err := filepath.Abs("../scripts/signing-key.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin, root, data := filepath.Join(dir, "bin"), filepath.Join(dir, "repo"), filepath.Join(dir, "data")
	pubkey := filepath.Join(root, "packaging/brig-release/RPM-GPG-KEY-brig")
	for _, d := range []string{bin, filepath.Dir(pubkey)} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"gh":  fakeGH,
		"git": "#!/bin/sh\necho '" + root + "'\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil { //nolint:gosec // G306: the scripts must be executable
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "log")
	env := append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+dir,
		"XDG_DATA_HOME="+data,
		"FAKE_LOG="+log,
		"FAKE_POLICIES=/dev/null",
		"BRIG_SIGNING_UID=test <test@invalid>",
	)
	run := func(args ...string) (string, error) {
		cmd := exec.Command(bash, append([]string{script}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	keys := func() []string {
		cmd := exec.Command("gpg", "--batch", "--with-colons", "--show-keys", pubkey)
		cmd.Env = append(os.Environ(), "GNUPGHOME="+t.TempDir())
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var fprs []string
		pub := false
		for line := range strings.SplitSeq(string(out), "\n") {
			f := strings.Split(line, ":")
			switch {
			case f[0] == "pub":
				pub = true
			case f[0] == "fpr" && pub:
				fprs, pub = append(fprs, f[9]), false
			}
		}
		return fprs
	}

	if out, err := run("--next"); err == nil {
		t.Fatalf("--next without a key succeeded:\n%s", out)
	}
	if out, err := run(); err != nil {
		t.Fatalf("create: %v\n%s", err, out)
	}
	first := keys()
	if out, err := run("--next"); err != nil {
		t.Fatalf("--next: %v\n%s", err, out)
	}
	both := keys()
	if len(first) != 1 || len(both) != 2 || both[0] != first[0] {
		t.Fatalf("keys after create %q, after --next %q", first, both)
	}
	next := both[1]

	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run("--switch", filepath.Join(data, "brig/signing-key", first[0]+".secret.asc")); err != nil {
		t.Fatalf("--switch to a listed key: %v\n%s", err, out)
	}
	if out, err := run("--switch", filepath.Join(data, "brig/signing-key", next+".secret.asc")); err != nil {
		t.Fatalf("--switch: %v\n%s", err, out)
	}
	if data, _ := os.ReadFile(log); strings.Count(string(data), "secret set RPM_SIGNING_KEY") != 2 {
		t.Errorf("gh calls:\n%s", data)
	}

	if out, err := run("--retire", first[0]); err != nil {
		t.Fatalf("--retire: %v\n%s", err, out)
	} else if !strings.Contains(out, "rpmkeys --delete "+strings.ToLower(first[0][32:])) {
		t.Errorf("--retire output:\n%s", out)
	}
	if got := keys(); len(got) != 1 || got[0] != next {
		t.Fatalf("keys after --retire %q, want %q", got, next)
	}
	if out, err := run("--retire", next); err == nil || !strings.Contains(out, "only key") {
		t.Errorf("retiring the last key: %v\n%s", err, out)
	}
	// The retired key is no longer one to switch to.
	if out, err := run("--switch", filepath.Join(data, "brig/signing-key", first[0]+".secret.asc")); err == nil || !strings.Contains(out, "no key that") {
		t.Errorf("--switch to a retired key: %v\n%s", err, out)
	}
}
