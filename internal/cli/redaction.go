package cli

import "regexp"

// Redaction covers recognized credential formats and home-directory usernames.

var (
	rePrivateKey = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
	reJWT        = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
	reGitHubTok  = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`)
	reBearer     = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{10,}`)
	// Secret-looking env assignment: NAME containing TOKEN/SECRET/KEY/PASSWORD = value.
	reSecretEnv = regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API[_-]?KEY|ACCESS[_-]?KEY|PRIVATE[_-]?KEY)[A-Za-z0-9_]*)["']?\s*[=:]\s*(?:"(?:\\.|[^"\\])*(?:"|$)|'(?:\\.|[^'\\])*(?:'|$)|[^\s"']{6,})`)
	// Absolute home paths: keep the path shape, drop the username. Two cases.
	//
	// Canonical home roots are unambiguous home directories wherever they appear —
	// inside file:// URLs, gitbash /c/Users paths, Windows C:\Users, sed
	// replacements — so they are stripped regardless of surrounding context:
	//   - macOS "/Users/<name>" or "\Users\<name>" (capital U is the OS-created dir)
	//   - Linux "/home/<name>"
	// Group 1 is the prefix incl. its trailing separator (preserved); the username
	// is dropped. Over-redacting a rare capital-Users source path is acceptable;
	// leaking a contributor's home username is not.
	reHomeCanonical = regexp.MustCompile(`((?:[/\\])Users[/\\]|/home/)[^/\\\s"']+`)
	// Lowercase "/users/" is overwhelmingly an API/repo path (api.github.com/users/<login>,
	// .../platform/users/components, users/me), so it is treated as a home dir only
	// when it starts a path token — start of string or after whitespace " ' = : ( , | —
	// which catches a lowercase-typed macOS home ("ls /users/<name>") while sparing
	// embedded API paths. Group 1 is the boundary, group 2 the "/users/" prefix.
	reHomeLowerUsers = regexp.MustCompile(`(^|[\s"'=:(,|])(/users/)[^/\s"']+`)
)

// redactText removes credential-shaped substrings and home-dir usernames.
func redactText(s string) string {
	if s == "" {
		return s
	}
	s = rePrivateKey.ReplaceAllString(s, "[REDACTED PRIVATE KEY]")
	s = reJWT.ReplaceAllString(s, "[REDACTED JWT]")
	s = reGitHubTok.ReplaceAllString(s, "[REDACTED TOKEN]")
	s = reBearer.ReplaceAllString(s, "Bearer [REDACTED]")
	s = reSecretEnv.ReplaceAllStringFunc(s, func(m string) string {
		sub := reSecretEnv.FindStringSubmatch(m)
		if len(sub) > 1 {
			return sub[1] + "=[REDACTED]"
		}
		return "[REDACTED]"
	})
	s = reHomeCanonical.ReplaceAllString(s, "${1}[redacted]")      // preserve prefix (e.g. /Users/), drop username
	s = reHomeLowerUsers.ReplaceAllString(s, "${1}${2}[redacted]") // preserve boundary + /users/, drop username
	return s
}

// redactStrings redacts each element of a slice (returns a new slice).
func redactStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = redactText(s)
	}
	return out
}

// redactCandidate returns a copy of a task candidate with every text-bearing
// evidence field redacted, for safe display/JSON egress.
func redactCandidate(c taskCandidate) taskCandidate {
	c.Label = redactText(c.Label)
	c.Commands = redactStrings(c.Commands)
	c.SampleIntents = redactStrings(c.SampleIntents)
	c.MatchingFacts = redactStrings(c.MatchingFacts)
	procs := make([]taskProcedure, len(c.Procedures))
	for i, p := range c.Procedures {
		p.Commands = redactStrings(p.Commands)
		procs[i] = p
	}
	c.Procedures = procs
	return c
}
