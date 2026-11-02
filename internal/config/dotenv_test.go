package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "\xef\xbb\xbfPEHREDAAR_TEST_BOM=yes\n" +
		"# secrets\n" +
		"PEHREDAAR_TEST_A=plain\n" +
		"\n" +
		"export PEHREDAAR_TEST_B = \"quoted value\"\n" +
		"PEHREDAAR_TEST_C='single'\n" +
		"PEHREDAAR_TEST_D=from-file\n" +
		"PEHREDAAR_TEST_E=a=b\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// t.Setenv restores the old value after the test. Keys LoadDotEnv sets
	// itself are registered here first so they are cleaned up too.
	for _, k := range []string{"PEHREDAAR_TEST_BOM", "PEHREDAAR_TEST_A", "PEHREDAAR_TEST_B", "PEHREDAAR_TEST_C", "PEHREDAAR_TEST_E"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("PEHREDAAR_TEST_D", "from-env") // the real environment wins

	set, err := LoadDotEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"PEHREDAAR_TEST_BOM": "yes",
		"PEHREDAAR_TEST_A":   "plain",
		"PEHREDAAR_TEST_B":   "quoted value",
		"PEHREDAAR_TEST_C":   "single",
		"PEHREDAAR_TEST_D":   "from-env",
		"PEHREDAAR_TEST_E":   "a=b",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if slices.Contains(set, "PEHREDAAR_TEST_D") || len(set) != 5 {
		t.Errorf("set = %v, want the 5 keys not already in the environment", set)
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	set, err := LoadDotEnv(filepath.Join(t.TempDir(), ".env"))
	if err != nil || set != nil {
		t.Fatalf("got %v, %v; want nil, nil", set, err)
	}
}

func TestLoadDotEnvBadLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("JUST_A_WORD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDotEnv(path); err == nil {
		t.Fatal("want an error for a line without =")
	}
}
