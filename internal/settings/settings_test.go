package settings

import "testing"

func TestValidate(t *testing.T) {
	if v, err := Validate("max_concurrent_base_copies", float64(8)); err != nil || v.(int64) != 8 {
		t.Fatalf("valid int: %v %v", v, err)
	}
	for _, bad := range []struct {
		k string
		v any
	}{
		{"max_concurrent_base_copies", float64(0)}, {"max_concurrent_base_copies", float64(33)}, {"max_concurrent_base_copies", 2.5},
		{"max_concurrent_base_copies", "8"}, {"heartbeat", "yes"}, {"decoding_plugin", "nope"}, {"no_such_setting", 1},
	} {
		if _, err := Validate(bad.k, bad.v); err == nil {
			t.Errorf("Validate(%s, %v) accepted", bad.k, bad.v)
		}
	}
	if _, err := Validate("decoding_plugin", "test_decoding"); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogDefaultsAreValid(t *testing.T) {
	for _, d := range Catalog {
		if _, err := Validate(d.Key, normalize(d.Default)); err != nil {
			t.Errorf("default of %s: %v", d.Key, err)
		}
		if d.Description == "" || d.Group == "" || (d.Scope != "global" && d.Scope != "migration") {
			t.Errorf("%s: incomplete definition", d.Key)
		}
	}
	if d, ok := Lookup("heartbeat"); !ok || d.Default != true {
		t.Fatal("heartbeat must default to on for online migrations")
	}
}

func normalize(v any) any {
	if i, ok := v.(int); ok {
		return float64(i)
	}
	return v
}

// D78: auto splits only when the largest table is 10 GB or more, into parts
// of the largest divided by the table jobs (whole GB, at least 1 GB).
func TestAutoSplit(t *testing.T) {
	const gb = int64(1) << 30
	for _, c := range []struct {
		largest int64
		jobs    int
		want    string
	}{
		{0, 8, ""},
		{9*gb + gb/2, 8, ""},
		{10 * gb, 8, "1GB"},
		{60 * gb, 8, "7GB"},
		{60 * gb, 0, "60GB"},
		{12 * gb, 16, "1GB"},
	} {
		if got := AutoSplit(c.largest, c.jobs); got != c.want {
			t.Errorf("AutoSplit(%d GB, %d) = %q, want %q", c.largest/gb, c.jobs, got, c.want)
		}
	}
}

func TestValidateSplitSize(t *testing.T) {
	for in, ok := range map[string]bool{"auto": true, "AUTO": true, "off": true, "50GB": true, "512MB": true, "1TB": true,
		"": false, "50": false, "50 GB": false, "abc": false, "0GB": false, "-1GB": false, "50gb": false} {
		_, err := Validate("split_tables_larger_than", in)
		if (err == nil) != ok {
			t.Errorf("Validate(%q): err %v, want ok=%v", in, err, ok)
		}
	}
}
