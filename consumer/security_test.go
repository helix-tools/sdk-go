package consumer

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

type securityRoundTripper struct {
	largeDownload bool
}

func (r securityRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body := "{}"
	contentLength := int64(-1)

	switch {
	case req.URL.Path == "/download-data":
		body = "download payload"
		contentLength = int64(len(body))
		if r.largeDownload {
			contentLength = 100*1024*1024 + 1
		}
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/download"):
		body = `{"download_url":"https://security.test/download-data"}`
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/v1/datasets/"):
		body = `{"_id":"ds-1","name":"test","metadata":{"compression_enabled":false,"encryption_enabled":false}}`
	}

	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: contentLength,
		Request:       req,
	}, nil
}

func newSecurityTestConsumer(largeDownload bool) *Consumer {
	return &Consumer{
		APIEndpoint: "https://security.test",
		CustomerID:  "test-customer",
		Region:      "us-east-1",
		awsConfig: aws.Config{
			Region:      "us-east-1",
			Credentials: credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", ""),
		},
		httpClient: &http.Client{Transport: securityRoundTripper{largeDownload: largeDownload}},
	}
}

func TestDownloadDatasetRejectsParentTraversal(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	if err := os.Mkdir(workDir, 0o750); err != nil {
		t.Fatalf("create work directory: %v", err)
	}
	t.Chdir(workDir)

	c := newSecurityTestConsumer(false)
	outputPath := filepath.Join("..", "escaped.bin")

	err := c.DownloadDataset(context.Background(), "ds-1", outputPath)
	if err == nil {
		t.Fatal("DownloadDataset accepted an output path that escapes its intended directory")
	}
	if !strings.Contains(err.Error(), "outside intended directory") {
		t.Fatalf("DownloadDataset error = %q, want containment error", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "escaped.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("escaped output was created or stat failed unexpectedly: %v", statErr)
	}
}

func TestCleanContainedPathRejectsAbsoluteParentTraversal(t *testing.T) {
	root := t.TempDir()
	path := root + string(os.PathSeparator) + "child" +
		string(os.PathSeparator) + ".." + string(os.PathSeparator) + "escaped.bin"

	if _, err := cleanContainedPath(path); err == nil {
		t.Fatal("cleanContainedPath accepted an absolute path containing parent traversal")
	}
}

func TestDownloadDatasetWritesPrivateFile(t *testing.T) {
	tests := []struct {
		name      string
		largePath bool
	}{
		{name: "small in-memory path"},
		{name: "large streaming path", largePath: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newSecurityTestConsumer(tt.largePath)

			outputPath := filepath.Join(t.TempDir(), "out.bin")
			if err := os.WriteFile(outputPath, []byte("old"), 0o644); err != nil {
				t.Fatalf("seed output file: %v", err)
			}
			if err := os.Chmod(outputPath, 0o644); err != nil {
				t.Fatalf("set seed permissions: %v", err)
			}

			if err := c.DownloadDataset(context.Background(), "ds-1", outputPath); err != nil {
				t.Fatalf("DownloadDataset: %v", err)
			}

			info, err := os.Stat(outputPath)
			if err != nil {
				t.Fatalf("stat output: %v", err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("output permissions = %04o, want 0600", got)
			}
		})
	}
}

func TestWriteFileWithinRootRejectsSymlinkEscape(t *testing.T) {
	destinationDir := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outsidePath, []byte("original"), 0o600); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}

	outputPath := filepath.Join(destinationDir, "output.bin")
	if err := os.Symlink(outsidePath, outputPath); err != nil {
		t.Fatalf("create escaping symlink: %v", err)
	}

	if err := writeFileWithinRoot(outputPath, []byte("replacement")); err == nil {
		t.Fatal("writeFileWithinRoot followed a symlink outside its root")
	}

	got, err := os.ReadFile(outsidePath)
	if err != nil {
		t.Fatalf("read outside file: %v", err)
	}
	if string(got) != "original" {
		t.Fatalf("outside file content = %q, want %q", got, "original")
	}
}
