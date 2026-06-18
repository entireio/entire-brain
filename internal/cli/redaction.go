package cli

import "regexp"

// Redaction (Pattern Consolidation, Priority 4).
//
// "No secrets" is an active boundary, not a passive rule: any evidence that
// leaves the brain — the synthesis agent's input, rendered cards, transcript
// excerpts, generated drafts, and JSON that includes evidence text — passes
// through redactText first. Patterns are deliberately high-precision; a missed
// false negative is worse than an occasional over-redaction.

var (
	rePrivateKey = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
	reJWT        = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
	reGitHubTok  = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`)
	reBearer     = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{10,}`)
	// Secret-looking env assignment: NAME containing TOKEN/SECRET/KEY/PASSWORD = value.
	reSecretEnv = regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API[_-]?KEY|ACCESS[_-]?KEY|PRIVATE[_-]?KEY)[A-Za-z0-9_]*)\s*[=:]\s*["']?[^\s"']{6,}`)
	// Absolute home paths: keep the path shape, drop the username.
	reUserHome = regexp.MustCompile(`/Users/[^/\s"']+`)
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
	s = reUserHome.ReplaceAllString(s, "/Users/[redacted]")
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
