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
