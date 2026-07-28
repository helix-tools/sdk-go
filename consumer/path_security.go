package consumer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func cleanContainedPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if filepath.IsAbs(path) {
		for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
			if segment == ".." {
				return "", fmt.Errorf("absolute path %q contains parent traversal", path)
			}
		}
	}

	cleanedPath := filepath.Clean(path)
	absolutePath, err := filepath.Abs(cleanedPath)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}

	intendedParent := filepath.Dir(absolutePath)
	if !filepath.IsAbs(path) {
		intendedParent, err = filepath.Abs(".")
		if err != nil {
			return "", fmt.Errorf("resolve intended directory: %w", err)
		}
	}

	resolvedParent, err := filepath.EvalSymlinks(intendedParent)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve intended directory: %w", err)
	}
	if os.IsNotExist(err) {
		resolvedParent = intendedParent
	}

	resolvedPath, err := filepath.EvalSymlinks(absolutePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve path: %w", err)
		}
		resolvedDirectory, resolveErr := filepath.EvalSymlinks(filepath.Dir(absolutePath))
		if resolveErr != nil && !os.IsNotExist(resolveErr) {
			return "", fmt.Errorf("resolve path directory: %w", resolveErr)
		}
		if os.IsNotExist(resolveErr) {
			resolvedDirectory = filepath.Dir(absolutePath)
		}
		resolvedPath = filepath.Join(resolvedDirectory, filepath.Base(absolutePath))
	}

	relativePath, err := filepath.Rel(resolvedParent, resolvedPath)
	if err != nil {
		return "", fmt.Errorf("validate path containment: %w", err)
	}
	if relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q is outside intended directory %q", path, intendedParent)
	}

	return absolutePath, nil
}

func writeFileWithinRoot(path string, data []byte) (err error) {
	parentDir := filepath.Dir(path)
	baseName := filepath.Base(path)

	root, err := os.OpenRoot(parentDir)
	if err != nil {
		return fmt.Errorf("open output directory: %w", err)
	}
	defer func() {
		if closeErr := root.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close output directory: %w", closeErr)
		}
	}()

	outputFile, err := root.OpenFile(baseName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open output file: %w", err)
	}
	defer func() {
		if closeErr := outputFile.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close output file: %w", closeErr)
		}
	}()

	if _, err = outputFile.Write(data); err != nil {
		return fmt.Errorf("write output file: %w", err)
	}
	if err = outputFile.Chmod(0o600); err != nil {
		return fmt.Errorf("secure output file permissions: %w", err)
	}

	return nil
}
