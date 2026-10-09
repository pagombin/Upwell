package pg

import (
	"strings"
	"testing"
)

func TestLSN(t *testing.T) {
	v, err := ParseLSN("16/B374D848")
	if err != nil || FormatLSN(v) != "16/B374D848" {
		t.Fatalf("round trip: %v %s", err, FormatLSN(v))
	}
	if _, err := ParseLSN("nonsense"); err == nil {
		t.Fatal("bad LSN accepted")
	}
	if d := LSNDiff("0/2000", "0/1000"); d != 0x1000 {
		t.Fatalf("diff %d", d)
	}
	if d := LSNDiff("1/0", "0/FFFFFFFF"); d != 1 {
		t.Fatalf("diff across the high word %d", d)
	}
	if d := LSNDiff("0/1000", "0/2000"); d != 0 {
		t.Fatalf("negative diff should be 0, got %d", d)
	}
	if !LSNGreaterOrEqual("0/2000", "0/2000") || LSNGreaterOrEqual("0/1FFF", "0/2000") || LSNGreaterOrEqual("bad", "0/0") {
		t.Fatal("LSNGreaterOrEqual")
	}
}

func TestValidDBName(t *testing.T) {
	for _, ok := range []string{"defaultdb", "orders_2024", "db-with-dash", strings.Repeat("a", 63)} {
		if !ValidDBName(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", 64), "x;DROP DATABASE y", "a b", "quote'name", `dq"name`, "--host"} {
		if ValidDBName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestQuote(t *testing.T) {
	if QuoteIdent(`we"ird`) != `"we""ird"` {
		t.Fatal(QuoteIdent(`we"ird`))
	}
	if QuoteLiteral("it's") != "'it''s'" {
		t.Fatal(QuoteLiteral("it's"))
	}
}
