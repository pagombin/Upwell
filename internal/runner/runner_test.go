package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteEnvFile(t *testing.T) {
	dir := t.TempDir()
	p, err := WriteEnvFile(Spec{RunDir: dir, Args: []string{"clone", "--follow", "--source", "postgres://doadmin@h:25060/db?sslmode=require"},
		Env: map[string]string{"PGOPTIONS": "-c synchronous_commit=off", "PGPASSFILE": filepath.Join(dir, "pgpass")}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	got := string(b)
	for _, want := range []string{
		`UPWELL_ENGINE_ARGS="clone --follow --source postgres://doadmin@h:25060/db?sslmode=require"`,
		`PGOPTIONS="-c synchronous_commit=off"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("env file lacks %s:\n%s", want, got)
		}
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if _, err := WriteEnvFile(Spec{RunDir: dir, Args: []string{"has space"}}); err == nil {
		t.Error("an argument with a space was accepted")
	}
	if envQuote(`a"b\c`) != `"a\"b\\c"` {
		t.Error(envQuote(`a"b\c`))
	}
}
