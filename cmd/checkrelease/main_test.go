package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectIncompleteRelease(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if check(dir) == nil {
		t.Fatal("incomplete release accepted")
	}
}
func TestZIPLayout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members []string
		ok      bool
	}{
		{"valid", []string{"vertex-adc.so"}, true},
		{"empty", nil, false},
		{"macos", []string{"vertex-adc.dylib"}, true},
		{"windows", []string{"vertex-adc.dll"}, true},
		{"windows-wrong-library", []string{"vertex-adc.so"}, false},
		{"removed-notices", []string{"vertex-adc.so", "THIRD_PARTY_NOTICES.txt"}, false},
		{"nested", []string{"folder/vertex-adc.so"}, false},
		{"extra", []string{"vertex-adc.so", "another.so"}, false},
		{"duplicate", []string{"vertex-adc.so", "vertex-adc.so"}, false},
		{"traversal", []string{"../vertex-adc.so"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "archive.zip")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			writer := zip.NewWriter(f)
			for _, name := range tc.members {
				part, err := writer.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = part.Write([]byte("fixture")); err != nil {
					t.Fatal(err)
				}
			}
			if err = writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
			goos := "linux"
			switch {
			case tc.name == "macos":
				goos = "darwin"
			case strings.HasPrefix(tc.name, "windows"):
				goos = "windows"
			}
			if err = checkZIP(path, goos); (err == nil) != tc.ok {
				t.Fatalf("checkZIP=%v want valid=%v", err, tc.ok)
			}
		})
	}
}
