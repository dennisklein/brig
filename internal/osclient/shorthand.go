// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/dennisklein/brig/internal/ui/store"
)

// A sandbox's supervisor sends each OCSF event to the gateway as a line of
// "shorthand", which OpenShell calls a display-only projection: no fields,
// only text. ParseRequest reads the lines about network traffic, as
// openshell-ocsf's format_shorthand writes them at OpenShell v0.1.4:
//
//	NET:OPEN [MED] DENIED /usr/bin/curl(4711) -> api.example.com:443 [reason:...]
//	HTTP:GET [INFO] ALLOWED curl(88) -> GET https://api.example.com/v1/data [policy:p engine:mechanistic]
//	HTTP:GET [MED] DENIED GET http://host:443/other [policy:p engine:l7] [reason:...]
//
// It is tolerant: every part but the class and the severity may be missing.
// A traffic line it cannot read comes back raw, never dropped, since a
// later OpenShell may write them differently.

// trafficClasses are the event classes whose lines tell of requests.
var trafficClasses = map[string]bool{"NET": true, "HTTP": true}

// otherClasses are the event classes that format_shorthand writes and that
// are not about network traffic: processes, SSH sessions into the sandbox,
// findings, config changes, the supervisor's life cycle, inference calls
// and plain events.
var otherClasses = map[string]bool{
	"PROC": true, "SSH": true, "FINDING": true, "CONFIG": true, "LIFECYCLE": true, "API": true, "EVENT": true,
}

var severityTags = map[string]bool{
	"[INFO]": true, "[LOW]": true, "[MED]": true, "[HIGH]": true, "[CRIT]": true, "[FATAL]": true,
}

var (
	// classRE matches the head of a line, CLASS:ACTIVITY.
	classRE = regexp.MustCompile(`^([A-Z][A-Z0-9_]*):([A-Z][A-Z0-9_]*)$`)
	// contextRE finds where the bracketed context fields start.
	contextRE = regexp.MustCompile(`(?:^| )\[[A-Za-z_][A-Za-z0-9_.-]*:`)
	// actorRE matches a process as name(pid).
	actorRE = regexp.MustCompile(`^(.+)\((-?[0-9]+)\)$`)
	// queryRE matches a URL's query, which may hold a token.
	queryRE = regexp.MustCompile(`\?[^\s\[\]]+`)
)

// Redact cuts the queries of URLs in s: the proxy logs the URL as the agent
// sent it, and a query may hold a token.
func Redact(s string) string { return queryRE.ReplaceAllString(s, "?…") }

// ParseRequest reads a line of a sandbox's OCSF log. It reports whether the
// line tells of network traffic; one that does but that it cannot read comes
// back with only Raw set.
func ParseRequest(line string) (store.Request, bool) {
	line = strings.TrimSpace(line)
	head, rest, _ := strings.Cut(line, " ")
	m := classRE.FindStringSubmatch(head)
	switch {
	case head == "EVENT", m != nil && otherClasses[m[1]]:
		return store.Request{}, false
	case m == nil || !trafficClasses[m[1]]:
		// Not a line format_shorthand writes, or a class this version does
		// not know: show it as it is.
		return store.Request{Raw: Redact(line)}, true
	}
	r, ok := parseTraffic(m[1], rest)
	switch {
	case !ok:
		return store.Request{Raw: Redact(line)}, true
	case r.Host == "" && r.Verdict != store.VerdictDeny:
		// Such as a relay opening: no request.
		return store.Request{}, false
	case r.Host == "":
		return store.Request{Raw: Redact(line)}, true
	}
	return r, true
}

// parseTraffic reads what follows CLASS:ACTIVITY.
func parseTraffic(class, rest string) (store.Request, bool) {
	var r store.Request
	sev, body, _ := strings.Cut(rest, " ")
	if !severityTags[sev] {
		return r, false
	}
	main, ctx := body, ""
	if loc := contextRE.FindStringIndex(body); loc != nil {
		main, ctx = body[:loc[0]], strings.TrimPrefix(body[loc[0]:], " ")
	}
	main = strings.TrimSpace(main)
	if word, after, _ := strings.Cut(main, " "); word != "" && isUpper(word) {
		switch word {
		case "ALLOWED":
			r.Verdict = store.VerdictAllow
		case "DENIED":
			r.Verdict = store.VerdictDeny
		}
		main = after
	}
	target := main
	if actor, dst, ok := strings.Cut(main, " -> "); ok {
		if !parseActor(&r, actor) {
			return r, false
		}
		target = dst
	} else if class == "NET" && actorRE.MatchString(main) {
		if !parseActor(&r, main) {
			return r, false
		}
		target = ""
	}
	if target != "" {
		parse := parseNetTarget
		if class == "HTTP" {
			parse = parseHTTPTarget
		}
		if !parse(&r, target) {
			return r, false
		}
	}
	return r, parseContext(&r, ctx)
}

func isUpper(s string) bool {
	return strings.Trim(s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ_") == ""
}

// parseActor reads name(pid). A name of "-" stands for a process the proxy
// could not tell.
func parseActor(r *store.Request, s string) bool {
	m := actorRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return false
	}
	if m[1] != "-" {
		r.Process = m[1]
	}
	r.PID, _ = strconv.Atoi(m[2])
	return true
}

// parseNetTarget reads host[:port][/protocol].
func parseNetTarget(r *store.Request, s string) bool {
	if strings.ContainsAny(s, " \t") {
		return false
	}
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	host := s
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		port, err := strconv.Atoi(s[i+1:])
		if err == nil && port > 0 && port < 65536 {
			host, r.Port = s[:i], port
		}
	}
	r.Host = host
	return host != ""
}

// parseHTTPTarget reads METHOD URL.
func parseHTTPTarget(r *store.Request, s string) bool {
	method, raw, ok := strings.Cut(s, " ")
	if !ok || !isUpper(method) || strings.ContainsAny(raw, " \t") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	r.Method, r.Host = method, u.Hostname()
	switch {
	case u.Port() != "":
		r.Port, err = strconv.Atoi(u.Port())
		if err != nil {
			return false
		}
	case u.Scheme == "https":
		r.Port = 443
	default:
		r.Port = 80
	}
	r.Path = u.EscapedPath()
	if r.Path == "" {
		r.Path = "/"
	}
	if u.RawQuery != "" || u.ForceQuery {
		r.Path += "?…"
	}
	return true
}

// parseContext reads the bracketed fields: [policy:NAME engine:KIND], other
// key:value fields, and the reason or a message, which run to the end of the
// line since OpenShell does not escape them.
func parseContext(r *store.Request, ctx string) bool {
	for ctx = strings.TrimSpace(ctx); ctx != ""; ctx = strings.TrimSpace(ctx) {
		if ctx[0] != '[' {
			return false
		}
		ctx = ctx[1:]
		end := closing(ctx)
		if end < 0 {
			return false
		}
		group := ctx[:end]
		if strings.HasPrefix(group, "msg:") {
			// A message, which OpenShell does not escape either, is last.
			return true
		}
		if i := reasonStart(group); i >= 0 {
			fields(r, ctx[:i])
			r.Reason = Redact(strings.TrimSuffix(strings.TrimSpace(ctx[i+len("reason:"):]), "]"))
			return true
		}
		fields(r, group)
		ctx = ctx[end+1:]
	}
	return true
}

// closing returns the index of the first "]" that the shorthand did not
// escape, or -1.
func closing(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case ']':
			return i
		}
	}
	return -1
}

// reasonStart returns where reason: starts in the group, -1 if the group
// has no reason. The reason may hold "]", so the group may end before the
// reason does.
func reasonStart(group string) int {
	switch {
	case strings.HasPrefix(group, "reason:"):
		return 0
	case strings.Contains(group, " reason:"):
		return strings.Index(group, " reason:") + 1
	}
	return -1
}

// fields reads key:value fields separated by spaces.
func fields(r *store.Request, s string) {
	for _, f := range strings.Fields(s) {
		k, v, _ := strings.Cut(f, ":")
		switch k {
		case "policy":
			if v != "-" {
				r.Policy = v
			}
		case "engine":
			r.Engine = v
		}
	}
}
