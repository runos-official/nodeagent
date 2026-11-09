package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReferenceModeRefusesProtectedGeneratedFileBeforeWrite(t *testing.T) {
	for _, tc := range []struct {
		name       string
		outputPath func(t *testing.T, root string) string
	}{
		{
			name: "direct path",
			outputPath: func(_ *testing.T, root string) string {
				return filepath.Join(root, "l2sec")
			},
		},
		{
			name: "equivalent path",
			outputPath: func(_ *testing.T, root string) string {
				return filepath.Join(root, "l2sec", "..", "l2sec")
			},
		},
		{
			name: "symlinked path",
			outputPath: func(t *testing.T, root string) string {
				alias := filepath.Join(root, "proto-output")
				if err := os.Symlink(filepath.Join(root, "l2sec"), alias); err != nil {
					t.Fatal(err)
				}
				return alias
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			protectedDir := filepath.Join(root, "l2sec")
			if err := os.MkdirAll(protectedDir, 0o755); err != nil {
				t.Fatal(err)
			}
			protectedPath := filepath.Join(root, generatedFile)
			const original = "proto contract comments must survive\n"
			if err := os.WriteFile(protectedPath, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("go", "run", ".", "-root", root, "-print", tc.outputPath(t, root))
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("reference mode succeeded for the protected file:\n%s", output)
			}
			if !strings.Contains(string(output), "refusing to overwrite protected generated file") {
				t.Fatalf("refusal output = %q", output)
			}
			got, readErr := os.ReadFile(protectedPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != original {
				t.Fatalf("protected file changed before refusal: got %d bytes, want %d", len(got), len(original))
			}
		})
	}
}
