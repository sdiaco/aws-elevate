package awsx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAliasOnceAndLeavesTheRest(t *testing.T) {
	for name, tc := range map[string]struct{ before, after string }{
		"new file":        {"", "[toplevel]\n" + AliasLine + "\n"},
		"other section":   {"[command s3]\nls = s3 ls\n", "[command s3]\nls = s3 ls\n\n[toplevel]\n" + AliasLine + "\n"},
		"no final break":  {"[command s3]\nls = s3 ls", "[command s3]\nls = s3 ls\n\n[toplevel]\n" + AliasLine + "\n"},
		"toplevel exists": {"[toplevel]\nwhoami = sts get-caller-identity\n\n[command s3]\nls = x\n", "[toplevel]\n" + AliasLine + "\nwhoami = sts get-caller-identity\n\n[command s3]\nls = x\n"},
		"header no break": {"[toplevel]", "[toplevel]\n" + AliasLine + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cli", "alias")
			if tc.before != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.before), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			changed, err := WriteAlias(path)
			if err != nil || !changed {
				t.Fatalf("changed %v, %v", changed, err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != tc.after {
				t.Errorf("got %q\nwant %q", got, tc.after)
			}
			changed, err = WriteAlias(path)
			again, _ := os.ReadFile(path)
			if err != nil || changed || string(again) != tc.after {
				t.Errorf("second write changed %v, %v: %q", changed, err, again)
			}
		})
	}
}

func TestWriteAliasKeepsAnExistingElevateAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alias")
	before := "[toplevel]\nelevate = !/opt/bin/aws-elevate\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := WriteAlias(path)
	got, _ := os.ReadFile(path)
	if err != nil || changed || string(got) != before {
		t.Errorf("changed %v, %v: %q", changed, err, got)
	}
}

func TestWriteAliasIgnoresElevateInOtherSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alias")
	if err := os.WriteFile(path, []byte("[command s3]\nelevate = x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := WriteAlias(path); err != nil || !changed {
		t.Errorf("changed %v, %v", changed, err)
	}
}
