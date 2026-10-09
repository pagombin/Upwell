package logx

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"postgres://doadmin:s3cr3t@db.example.com:25060/defaultdb": "s3cr3t",
		"password=hunter2 host=x":                                  "hunter2",
		`PGPASSWORD="abc def" pg_dump`:                             "abc def",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig":   "eyJhbGciOiJIUzI1NiJ9",
		"posting to https://hooks.slack.com/services/T0/B0/XYZ":    "T0/B0/XYZ",
		"token dop_v1_0123456789abcdef0123456789abcdef":            "0123456789abcdef0123456789abcdef",
		"api_key: AKIAIOSFODNN7EXAMPLE":                            "AKIAIOSFODNN7EXAMPLE",
	}
	for in, secret := range cases {
		out := Redact(in)
		if strings.Contains(out, secret) {
			t.Errorf("Redact(%q) = %q still contains the secret", in, out)
		}
		if !strings.Contains(out, "****") {
			t.Errorf("Redact(%q) = %q has no mask", in, out)
		}
	}
	if got := Redact("copied 42 rows into public.orders"); got != "copied 42 rows into public.orders" {
		t.Errorf("ordinary text changed: %q", got)
	}
}
