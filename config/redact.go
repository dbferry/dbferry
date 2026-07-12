package config

import "strings"

// Redactor collects every resolved secret (DB password, S3 access/secret/session
// token) so they can be scrubbed from any output before it is printed or logged
// (ADR-0004). Add secrets as they are resolved, then wrap all output in Redact.
type Redactor struct {
	secrets []string
}

// Add registers secrets to scrub. Empty strings are ignored.
func (r *Redactor) Add(secrets ...string) {
	for _, s := range secrets {
		if s != "" {
			r.secrets = append(r.secrets, s)
		}
	}
}

// Redact replaces every registered secret with a placeholder.
func (r *Redactor) Redact(s string) string {
	for _, sec := range r.secrets {
		s = strings.ReplaceAll(s, sec, "[redacted]")
	}
	return s
}
