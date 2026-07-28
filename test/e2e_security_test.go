package main

import (
	"os"
	"strings"
	"testing"
)

func TestE2EFixtureUsesRestrictedPermissionsAndHandlesErrors(t *testing.T) {
	source, err := os.ReadFile("e2e_main.go")
	if err != nil {
		t.Fatalf("read e2e_main.go: %v", err)
	}
	code := string(source)

	for _, forbidden := range []string{
		"os.MkdirAll(tmpDir, 0755)",
		"os.WriteFile(testFilePath, jsonData, 0644)",
		"\n\tos.Remove(testFilePath)",
	} {
		if strings.Contains(code, forbidden) {
			t.Errorf("e2e_main.go still contains insecure or unchecked operation %q", forbidden)
		}
	}
	if !strings.Contains(code, "os.MkdirAll(tmpDir, 0750)") {
		t.Error("e2e_main.go does not create the fixture directory with 0750")
	}
	if !strings.Contains(code, "os.WriteFile(testFilePath, jsonData, 0600)") {
		t.Error("e2e_main.go does not create the fixture file with 0600")
	}
}
