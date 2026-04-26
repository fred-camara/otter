package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

//go:embed pdfkit_extract.swift
var pdfKitExtractorScript string

type pdfKitPageResult struct {
	PageNumber int    `json:"pageNumber"`
	Text       string `json:"text"`
	CharCount  int    `json:"charCount"`
	LineCount  int    `json:"lineCount"`
}

type pdfKitDocumentResult struct {
	SourcePath                string             `json:"sourcePath"`
	PageCount                 int                `json:"pageCount"`
	Encrypted                 bool               `json:"encrypted"`
	Locked                    bool               `json:"locked"`
	UnlockAttempted           bool               `json:"unlockAttempted"`
	UnlockedWithEmptyPassword bool               `json:"unlockedWithEmptyPassword"`
	Pages                     []pdfKitPageResult `json:"pages"`
}

var pdfKitScriptOnce sync.Once
var pdfKitScriptPath string
var pdfKitScriptErr error

func pdfKitAvailable() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath("swift")
	return err == nil
}

func extractWithPDFKit(ctx context.Context, path string) (*pdfKitDocumentResult, error) {
	if !pdfKitAvailable() {
		return nil, fmt.Errorf("pdfkit unavailable")
	}
	scriptPath, err := ensurePDFKitScript()
	if err != nil {
		return nil, err
	}

	cacheRoot := filepath.Join(os.TempDir(), "otter-swift")
	cacheDir := filepath.Join(cacheRoot, "clang-cache")
	homeDir := filepath.Join(cacheRoot, "home")
	tmpDir := filepath.Join(cacheRoot, "tmp")
	for _, dir := range []string{cacheDir, homeDir, tmpDir} {
		if mkdirErr := os.MkdirAll(dir, 0o755); mkdirErr != nil {
			return nil, mkdirErr
		}
	}

	cmd := exec.CommandContext(ctx, "swift", scriptPath, path)
	cmd.Env = append(os.Environ(),
		"HOME="+homeDir,
		"CLANG_MODULE_CACHE_PATH="+cacheDir,
		"TMPDIR="+tmpDir,
	)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			message := strings.TrimSpace(string(exitErr.Stderr))
			if message == "" {
				message = exitErr.Error()
			}
			return nil, fmt.Errorf("pdfkit extractor failed: %s", message)
		}
		return nil, err
	}

	var result pdfKitDocumentResult
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("parse pdfkit output: %w", err)
	}
	return &result, nil
}

func ensurePDFKitScript() (string, error) {
	pdfKitScriptOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "otter-swift")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			pdfKitScriptErr = err
			return
		}
		path := filepath.Join(dir, "pdfkit_extract.swift")
		if err := os.WriteFile(path, []byte(pdfKitExtractorScript), 0o644); err != nil {
			pdfKitScriptErr = err
			return
		}
		pdfKitScriptPath = path
	})
	return pdfKitScriptPath, pdfKitScriptErr
}
