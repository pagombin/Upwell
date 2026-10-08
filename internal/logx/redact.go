package logx

import "regexp"

var redactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	// passwords in connection URIs: scheme://user:secret@host
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/@\s]*:)[^@\s]*@`), `${1}****@`},
	// key=value and key: value pairs
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|pgpassword|secret|token|api[_-]?key)(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&]+)`), `${1}${2}****`},
	// bearer headers
	{regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]+`), `${1} ****`},
	// webhook URLs (Slack and generic hooks)
	{regexp.MustCompile(`https://hooks\.slack\.com/[^\s"']+`), `https://hooks.slack.com/****`},
	// DigitalOcean API tokens
	{regexp.MustCompile(`\bdop_v1_[a-f0-9]{16,}\b`), `dop_v1_****`},
}

// Redact masks secrets in s. It runs before any log write.
func Redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
