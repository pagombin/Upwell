package auth

import (
	"encoding/base32"
	"testing"
	"time"
)

// TestTOTPVectors checks the RFC 6238 appendix B SHA-1 vectors (last six digits).
func TestTOTPVectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for _, v := range []struct {
		unix int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"}} {
		got, err := TOTPCode(secret, TOTPStep(time.Unix(v.unix, 0)))
		if err != nil || got != v.want {
			t.Errorf("t=%d: %s, want %s (%v)", v.unix, got, v.want, err)
		}
	}
	if matchTOTP(secret, "287082", time.Unix(59, 0), 0) == 0 {
		t.Error("the current code did not match")
	}
	if matchTOTP(secret, "287082", time.Unix(59+90, 0), 0) != 0 {
		t.Error("a code three steps old matched")
	}
}
