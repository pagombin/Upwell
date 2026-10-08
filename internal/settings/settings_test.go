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
