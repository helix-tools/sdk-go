package producer

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnalyzeDataRejectsParentTraversal(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	if err := os.Mkdir(workDir, 0o750); err != nil {
		t.Fatalf("create work directory: %v", err)
	}
	outsidePath := filepath.Join(root, "outside.ndjson")
	if err := os.WriteFile(outsidePath, []byte("{\"id\":1}\n"), 0o600); err != nil {
		t.Fatalf("create outside input: %v", err)
	}
	t.Chdir(workDir)

	_, err := (&Producer{}).analyzeData(filepath.Join("..", "outside.ndjson"), DefaultAnalysisOptions())
	if err == nil {
		t.Fatal("analyzeData accepted an input path that escapes its intended directory")
	}
	if !strings.Contains(err.Error(), "outside intended directory") {
		t.Fatalf("analyzeData error = %q, want containment error", err)
	}
}

func TestProcessFileRejectsParentTraversal(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	if err := os.Mkdir(workDir, 0o750); err != nil {
		t.Fatalf("create work directory: %v", err)
	}
	outsidePath := filepath.Join(root, "outside.ndjson")
	if err := os.WriteFile(outsidePath, nil, 0o600); err != nil {
		t.Fatalf("create outside input: %v", err)
	}
	t.Chdir(workDir)

	p := &Producer{KMSKeyID: "test-key"}
	_, err := p.processFile(
		context.Background(),
		filepath.Join("..", "outside.ndjson"),
		NewUploadOptions("test-dataset"),
	)
	if err == nil {
		t.Fatal("processFile accepted an input path that escapes its intended directory")
	}
	if !strings.Contains(err.Error(), "outside intended directory") {
		t.Fatalf("processFile error = %q, want containment error", err)
	}
}

func TestCleanContainedPathRejectsAbsoluteParentTraversal(t *testing.T) {
	root := t.TempDir()
	path := root + string(os.PathSeparator) + "child" +
		string(os.PathSeparator) + ".." + string(os.PathSeparator) + "outside.ndjson"

	if _, err := cleanContainedPath(path); err == nil {
		t.Fatal("cleanContainedPath accepted an absolute path containing parent traversal")
	}
}

func TestEncryptedKeyLengthConversionIsBoundsChecked(t *testing.T) {
	got, err := checkedEncryptedKeyLength(math.MaxUint32)
	if err != nil {
		t.Fatalf("maximum uint32 length rejected: %v", err)
	}
	if got != math.MaxUint32 {
		t.Fatalf("checked length = %d, want %d", got, uint32(math.MaxUint32))
	}

	if _, err := checkedEncryptedKeyLength(uint64(math.MaxUint32) + 1); err == nil {
		t.Fatal("length larger than math.MaxUint32 was accepted")
	}
}
