package orch

import (
	"strconv"
	"strings"
)

// parseNetDev sums received and sent bytes over every interface in
// /proc/net/dev except the loopback (overnight item 11).
func parseNetDev(b []byte) (rx, tx uint64, ok bool) {
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || name == "" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, err1 := strconv.ParseUint(f[0], 10, 64)
		t, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx, tx, ok = rx+r, tx+t, true
	}
	return rx, tx, ok
}
