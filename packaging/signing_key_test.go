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
