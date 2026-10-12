// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"testing"

	"github.com/dennisklein/brig/internal/ui/store"
)

func TestParseRequest(t *testing.T) {
	tests := []struct {
		line string
		want store.Request
	}{
		// Lines from OpenShell's own tests at v0.1.3: openshell-ocsf's
		// format/shorthand.rs, openshell-cli's run.rs and
		// openshell-supervisor-network's policy_local.rs.
		{
			"NET:OPEN [INFO] ALLOWED python3(42) -> api.example.com:443 [policy:default-egress engine:mechanistic]",
			store.Request{Verdict: "allow", Process: "python3", PID: 42, Host: "api.example.com", Port: 443, Policy: "default-egress", Engine: "mechanistic"},
		},
		{
			"NET:REFUSE [MED] DENIED node(1234) -> 93.184.216.34:443/tcp [policy:bypass-detect engine:nftables]",
			store.Request{Verdict: "deny", Process: "node", PID: 1234, Host: "93.184.216.34", Port: 443, Policy: "bypass-detect", Engine: "nftables"},
		},
		{
			"NET:OPEN [MED] DENIED /usr/bin/curl(0) -> blocked.invalid:80 [reason:transparent_tcp_policy_denied]",
			store.Request{Verdict: "deny", Process: "/usr/bin/curl", Host: "blocked.invalid", Port: 80, Reason: "transparent_tcp_policy_denied"},
		},
		{
			"NET:OPEN [MED] DENIED /usr/bin/curl(4711) -> api.example.com:443",
			store.Request{Verdict: "deny", Process: "/usr/bin/curl", PID: 4711, Host: "api.example.com", Port: 443},
		},
		{
			"NET:REFUSE [MED] DENIED blocked.invalid [reason:policy_dns_ineligible]",
			store.Request{Verdict: "deny", Host: "blocked.invalid", Reason: "policy_dns_ineligible"},
		},
		{
			"HTTP:GET [INFO] ALLOWED curl(88) -> GET https://api.example.com/v1/data [policy:default-egress engine:mechanistic]",
			store.Request{Verdict: "allow", Method: "GET", Process: "curl", PID: 88, Host: "api.example.com", Port: 443, Path: "/v1/data", Policy: "default-egress", Engine: "mechanistic"},
		},
		{
			"HTTP:POST [INFO] ALLOWED POST http://httpbin.org:443/anything [policy:httpbin engine:extension] [attempt:2 cached:true]",
			store.Request{Verdict: "allow", Method: "POST", Host: "httpbin.org", Port: 443, Path: "/anything", Policy: "httpbin", Engine: "extension"},
		},
		{
			"HTTP:GET [INFO] ALLOWED curl(68) -> GET http://172.20.0.1:9876/test [policy:allow_host_9876 engine:mechanistic]",
			store.Request{Verdict: "allow", Method: "GET", Process: "curl", PID: 68, Host: "172.20.0.1", Port: 9876, Path: "/test", Policy: "allow_host_9876", Engine: "mechanistic"},
		},
		{
			"HTTP:GET [MED] DENIED GET http://gateway.example.test:443/other [policy:route_api engine:l7] [reason:L7_REQUEST deny GET gateway.example.test:443/other reason=no L7 endpoint path matched request]",
			store.Request{
				Verdict: "deny", Method: "GET", Host: "gateway.example.test", Port: 443, Path: "/other", Policy: "route_api", Engine: "l7",
				Reason: "L7_REQUEST deny GET gateway.example.test:443/other reason=no L7 endpoint path matched request",
			},
		},
		{
			"HTTP:PUT [MED] DENIED PUT http://api.github.com/x [policy:p engine:opa] [reason:FORWARD denied PUT api.github.com:443/x?token=secret-456]",
			store.Request{Verdict: "deny", Method: "PUT", Host: "api.github.com", Port: 80, Path: "/x", Policy: "p", Engine: "opa", Reason: "FORWARD denied PUT api.github.com:443/x?…"},
		},
		// Queries may hold tokens: they are cut from the path and the reason.
		{
			"HTTP:GET [MED] DENIED node(7) -> GET https://registry.npmjs.org/@types%2fnode?access_token=s3cret [policy:- engine:opa] [reason:endpoint registry.npmjs.org:443 is not allowed by any policy]",
			store.Request{
				Verdict: "deny", Method: "GET", Process: "node", PID: 7, Host: "registry.npmjs.org", Port: 443, Path: "/@types%2fnode?…", Engine: "opa",
				Reason: "endpoint registry.npmjs.org:443 is not allowed by any policy",
			},
		},
		// Unmapped fields come before the reason in one group, and the
		// reason may hold brackets.
		{
			"HTTP:POST [MED] DENIED claude(9) -> POST https://uploads.github.com/repos/a/b [policy:_provider_github engine:l7] [attempt:1 reason:credential_endpoint_mismatch [GH_TOKEN] not bound]",
			store.Request{
				Verdict: "deny", Method: "POST", Process: "claude", PID: 9, Host: "uploads.github.com", Port: 443, Path: "/repos/a/b", Policy: "_provider_github", Engine: "l7",
				Reason: "credential_endpoint_mismatch [GH_TOKEN] not bound",
			},
		},
		{
			"NET:OPEN [MED] DENIED -(0) -> 10.0.2.2:11434 [policy:- engine:opa] [reason:endpoint 10.0.2.2:11434 is not allowed by any policy]",
			store.Request{Verdict: "deny", Host: "10.0.2.2", Port: 11434, Engine: "opa", Reason: "endpoint 10.0.2.2:11434 is not allowed by any policy"},
		},
	}
	for _, tt := range tests {
		got, ok := ParseRequest(tt.line)
		if !ok || got != tt.want {
			t.Errorf("ParseRequest(%q)\n got %+v, %v\nwant %+v", tt.line, got, ok, tt.want)
		}
	}
}

func TestParseRequestRaw(t *testing.T) {
	// Lines about traffic that do not read as format_shorthand writes them
	// are kept as they are, queries cut.
	for line, raw := range map[string]string{
		"NET:OPEN [MED] DENIED":                               "NET:OPEN [MED] DENIED",
		"NET:OPEN DENIED curl(1) -> a.example:443":            "NET:OPEN DENIED curl(1) -> a.example:443",
		"HTTP:GET [MED] DENIED GET not-a-url":                 "HTTP:GET [MED] DENIED GET not-a-url",
		"HTTP:GET [MED] DENIED GET ftp://a.example/x?k=v":     "HTTP:GET [MED] DENIED GET ftp://a.example/x?…",
		"NET:OPEN [INFO] ALLOWED curl(1) -> a.example:443 [x": "NET:OPEN [INFO] ALLOWED curl(1) -> a.example:443 [x",
		"DNS:QUERY [INFO] ALLOWED a.example":                  "DNS:QUERY [INFO] ALLOWED a.example",
		"something else entirely":                             "something else entirely",
	} {
		got, ok := ParseRequest(line)
		if !ok || got != (store.Request{Raw: raw}) {
			t.Errorf("ParseRequest(%q) = %+v, %v; want raw %q", line, got, ok, raw)
		}
	}
}

func TestParseRequestNotTraffic(t *testing.T) {
	for _, line := range []string{
		// From OpenShell's shorthand tests: not about requests.
		"NET:OPEN [INFO] [msg:relay open (channel_id=ch-42)]",
		"SSH:OPEN [INFO] ALLOWED 10.42.0.1:48201 [auth:NSSH1]",
		"PROC:LAUNCH [INFO] python3(42) [cmd:python3 /app/main.py]",
		"PROC:TERMINATE [INFO] python3(42) [exit:0]",
		`FINDING:BLOCKED [HIGH] "NSSH1 Nonce Replay Attack" [type:nssh1-replay-abc confidence:high]`,
		"LIFECYCLE:START [INFO] openshell-sandbox success",
		"CONFIG:LOADED [INFO] policy reloaded [version:v3 hash:sha256:abc123def456]",
		"EVENT [INFO] Network namespace created [ns:openshell-sandbox-abc123]",
		"API:INFERENCE [INFO] success gpt-4 via openai 120ms [chat]",
	} {
		if got, ok := ParseRequest(line); ok {
			t.Errorf("ParseRequest(%q) = %+v, want no request", line, got)
		}
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{ //nolint:gosec // G101: made-up tokens, to cut
		"GET https://a.example/x?token=s3cret&b=1 done": "GET https://a.example/x?… done",
		"[reason:FORWARD denied PUT a:443/x?t=1]":       "[reason:FORWARD denied PUT a:443/x?…]",
		"why? because": "why? because",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}
