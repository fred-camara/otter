package agent

import (
	"strings"
	"testing"

	"otter/internal/tools"
)

func TestBuildChunkSummaryPromptUsesLeanSchema(t *testing.T) {
	doc := &tools.ExtractedDocument{
		Warnings: []tools.ExtractionWarning{
			{Code: "ocr_unavailable", Message: "ocr unavailable", PageNumber: 1},
		},
	}
	chunk := tools.DocumentChunk{
		PageStart: 1,
		PageEnd:   1,
		Kind:      "text_native",
		Text:      "Candidate worked at Datadog from 2010 to present.",
	}

	prompt := buildChunkSummaryPrompt("/tmp/profile.pdf", chunk, doc)

	for _, expected := range []string{
		"1. Summary",
		"2. Key Facts",
		"3. Open Questions",
		"Source: /tmp/profile.pdf",
		"Pages: 1-1",
		"Warnings: page 1 ocr_unavailable: ocr unavailable",
		"Focus on the most useful facts and avoid repeating the chunk verbatim.",
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("expected prompt to contain %q, got: %s", expected, prompt)
		}
	}

	for _, unexpected := range []string{
		"4. Dates",
		"Financials",
		"Obligations",
		"Table Summaries",
		"Page Refs",
	} {
		if strings.Contains(prompt, unexpected) {
			t.Fatalf("did not expect prompt to contain %q, got: %s", unexpected, prompt)
		}
	}
}

func TestBuildChunkSummaryPromptAddsTableGuidanceForTableLikeChunks(t *testing.T) {
	prompt := buildChunkSummaryPrompt("/tmp/table.pdf", tools.DocumentChunk{
		PageStart: 2,
		PageEnd:   2,
		Kind:      "table_like",
		Text:      "Revenue | 2024 | 1200000",
	}, nil)

	if !strings.Contains(prompt, "include a short 'Tables' subsection under Key Facts") {
		t.Fatalf("expected table guidance in prompt, got: %s", prompt)
	}
}
