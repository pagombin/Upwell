package pgpack

import "testing"

func TestEngineMajorMinor(t *testing.T) {
	for _, c := range []struct {
		in       string
		maj, min int
		ok       bool
	}{
		{"0.18", 0, 18, true},
		{"0.18.1", 0, 18, true},
		{"0.18-1.pgdg24.04+1", 0, 18, true}, // PGDG package on Ubuntu 24.04 (the droplet)
		{"0.17-1.pgdg22.04+1", 0, 17, true},
		{"0.13-1", 0, 13, true},
		{"1.0.0", 1, 0, true},
		{"v0.18", 0, 18, true},
		{"", 0, 0, false},
		{"unknown", 0, 0, false},
	} {
		maj, min, ok := EngineMajorMinor(c.in)
		if maj != c.maj || min != c.min || ok != c.ok {
			t.Errorf("%q: got %d.%d ok=%v, want %d.%d ok=%v", c.in, maj, min, ok, c.maj, c.min, c.ok)
		}
	}
}

func TestSettingMillis(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "30000": 30000, "5min": 300000, "1h": 3600000, "1d": 86400000, "100ms": 100, "30s": 30000, "'10min'": 600000} {
		if got := settingMillis(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}

func TestToolMajorRegexp(t *testing.T) {
	for in, want := range map[string]string{
		"pg_dump (PostgreSQL) 18.0 (Ubuntu 18.0-1.pgdg24.04+1)\n":       "18",
		"pg_restore (PostgreSQL) 16.15 (Ubuntu 16.15-0ubuntu0.24.04.1)": "16",
		"pg_dump (PostgreSQL) 18.6":                                     "18",
	} {
		if m := majorRe.FindStringSubmatch(in); m == nil || m[1] != want {
			t.Errorf("%q: %v, want %s", in, m, want)
		}
	}
}

func TestEngineCommitOrderFixed(t *testing.T) {
	for v, want := range map[string]bool{
		"0.18": false, "0.18-1.pgdg24.04+1": false, "0.19": false, "0.18.dirty": false,
		"0.18+upwell.0": false, "0.18+upwell.1": true, "0.18+upwell.2": true, "0.18+upwell.3": true, "0.19+upwell.1": true,
	} {
		if got := EngineCommitOrderFixed(v); got != want {
			t.Errorf("EngineCommitOrderFixed(%q) = %v, want %v", v, got, want)
		}
	}
}

// D103: builds before upwell.4 wait 100 ms before applying each transaction
// (at most about ten a second); they lose nothing, so preflight warns.
func TestEngineApplyUnthrottled(t *testing.T) {
	for v, want := range map[string]bool{
		"0.18": false, "0.18+upwell.1": false, "0.18+upwell.3": false, "0.18+upwell.4": true, "0.18+upwell.12": true, "0.19+upwell.4": true,
	} {
		if got := EngineApplyUnthrottled(v); got != want {
			t.Errorf("EngineApplyUnthrottled(%q) = %v, want %v", v, got, want)
		}
	}
}
