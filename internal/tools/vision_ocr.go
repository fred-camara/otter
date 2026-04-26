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
	"strconv"
	"sync"
)

//go:embed vision_ocr.swift
var visionOCRScript string

type visionOCRResponse struct {
	PageNumber int     `json:"pageNumber"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
}

type VisionOCRProvider struct{}

var visionOCRScriptOnce sync.Once
var visionOCRScriptPath string
var visionOCRScriptErr error

func (VisionOCRProvider) Name() string {
	return "vision-swift"
}

func (VisionOCRProvider) OCRPage(ctx context.Context, req OCRPageRequest) (OCRResult, error) {
	if runtime.GOOS != "darwin" {
		return OCRResult{}, fmt.Errorf("vision ocr only supported on macos")
	}
	if _, err := exec.LookPath("swift"); err != nil {
		return OCRResult{}, fmt.Errorf("swift not available")
	}
	scriptPath, err := ensureVisionOCRScript()
	if err != nil {
		return OCRResult{}, err
	}

	cacheRoot := filepath.Join(os.TempDir(), "otter-swift")
	cacheDir := filepath.Join(cacheRoot, "clang-cache")
	homeDir := filepath.Join(cacheRoot, "home")
	tmpDir := filepath.Join(cacheRoot, "tmp")
	for _, dir := range []string{cacheDir, homeDir, tmpDir} {
		if mkdirErr := os.MkdirAll(dir, 0o755); mkdirErr != nil {
			return OCRResult{}, mkdirErr
		}
	}

	cmd := exec.CommandContext(ctx, "swift", scriptPath, req.SourcePath, strconv.Itoa(req.PageNumber))
	cmd.Env = append(os.Environ(),
		"HOME="+homeDir,
		"CLANG_MODULE_CACHE_PATH="+cacheDir,
		"TMPDIR="+tmpDir,
	)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return OCRResult{}, fmt.Errorf("vision ocr failed: %s", string(exitErr.Stderr))
		}
		return OCRResult{}, err
	}

	var parsed visionOCRResponse
	if err := json.Unmarshal(output, &parsed); err != nil {
		return OCRResult{}, fmt.Errorf("parse vision ocr output: %w", err)
	}
	return OCRResult{
		Text:       parsed.Text,
		Confidence: parsed.Confidence,
	}, nil
}

func ensureVisionOCRScript() (string, error) {
	visionOCRScriptOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "otter-swift")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			visionOCRScriptErr = err
			return
		}
		path := filepath.Join(dir, "vision_ocr.swift")
		if err := os.WriteFile(path, []byte(visionOCRScript), 0o644); err != nil {
			visionOCRScriptErr = err
			return
		}
		visionOCRScriptPath = path
	})
	return visionOCRScriptPath, visionOCRScriptErr
}
