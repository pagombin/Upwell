package api

import (
	"regexp"
	"testing"
	"time"
)

// Every download has its own name: scope, UTC time and suffix, safe characters only.
func TestDownloadName(t *testing.T) {
	n := downloadName([]string{"logs", "mfqv4x", "migration_poc", "", "a/b c"}, "-to42.jsonl")
	if !regexp.MustCompile(`^upwell-logs-mfqv4x-migration_poc-a_b_c-\d{8}T\d{6}Z-to42\.jsonl$`).MatchString(n) {
		t.Fatalf("name %q", n)
	}
	a := downloadName([]string{"audit"}, ".jsonl")
	time.Sleep(1100 * time.Millisecond)
	if b := downloadName([]string{"audit"}, ".jsonl"); a == b {
		t.Fatalf("two downloads a second apart share the name %q", a)
	}
}
