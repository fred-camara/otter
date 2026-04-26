package agent

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"otter/internal/tools"
)

const defaultChunkSummaryConcurrency = 1

type chunkSummaryTask struct {
	index int
	path  string
	chunk tools.DocumentChunk
	doc   *tools.ExtractedDocument
}

type chunkSummaryResult struct {
	index   int
	path    string
	chunk   tools.DocumentChunk
	output  string
	err     error
	warning string
}

func (o *Orchestrator) summarizeDocumentsWithModel(task string, docs []*tools.ExtractedDocument) (string, error) {
	tasks := buildChunkSummaryTasks(docs)
	if len(tasks) == 0 {
		return "", fmt.Errorf("no document chunks available for model summary")
	}

	results := make([]chunkSummaryResult, len(tasks))
	jobs := make(chan chunkSummaryTask)
	out := make(chan chunkSummaryResult, len(tasks))
	workers := o.modelSummaryWorkers
	if workers < 1 {
		workers = defaultChunkSummaryConcurrency
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range jobs {
				o.emitProgress(fmt.Sprintf("Summarizing chunk %d/%d (%s pages %d-%d)", task.index+1, len(tasks), filepath.Base(task.path), task.chunk.PageStart, task.chunk.PageEnd))
				started := time.Now()
				prompt := buildChunkSummaryPrompt(task.path, task.chunk, task.doc)
				output, err := o.generateModelSummaryWithTimeout(prompt)
				result := chunkSummaryResult{
					index: task.index,
					path:  task.path,
					chunk: task.chunk,
					err:   err,
				}
				if err == nil {
					result.output = strings.TrimSpace(output)
					if result.output == "" {
						result.err = fmt.Errorf("model returned empty output")
					}
				}
				if result.err != nil {
					result.warning = fmt.Sprintf("%s pages %d-%d: %v", filepath.Base(task.path), task.chunk.PageStart, task.chunk.PageEnd, result.err)
					o.emitProgress(fmt.Sprintf("Chunk %d/%d failed after %s: %v", task.index+1, len(tasks), time.Since(started).Round(100*time.Millisecond), result.err))
				} else {
					o.emitProgress(fmt.Sprintf("Completed chunk %d/%d in %s", task.index+1, len(tasks), time.Since(started).Round(100*time.Millisecond)))
					if preview := chunkProgressPreview(result.output); preview != "" {
						o.emitProgress(fmt.Sprintf("Partial summary %d/%d: %s", task.index+1, len(tasks), preview))
					}
				}
				out <- result
			}
		}()
	}

	go func() {
		for _, task := range tasks {
			jobs <- task
		}
		close(jobs)
		wg.Wait()
		close(out)
	}()

	successes := 0
	warnings := make([]string, 0, len(tasks))
	var firstErr error
	for result := range out {
		results[result.index] = result
		if result.err == nil {
			successes++
		} else if result.warning != "" {
			warnings = append(warnings, result.warning)
			if firstErr == nil {
				firstErr = result.err
			}
		}
	}
	if successes == 0 {
		if firstErr != nil {
			return "", firstErr
		}
		return "", fmt.Errorf("all chunk summaries failed")
	}
	if successes == 1 {
		for _, result := range results {
			if result.err != nil || strings.TrimSpace(result.output) == "" {
				continue
			}
			final := strings.TrimSpace(result.output)
			if len(warnings) > 0 {
				final += "\n\nWarnings:\n- " + strings.Join(warnings, "\n- ")
			}
			return final, nil
		}
	}

	o.emitProgress(fmt.Sprintf("Merging %d chunk summary result(s)", successes))
	mergePrompt := buildChunkMergePrompt(task, docs, results)
	merged, err := o.generateModelSummaryWithTimeout(mergePrompt)
	if err == nil && strings.TrimSpace(merged) != "" {
		final := strings.TrimSpace(merged)
		if len(warnings) > 0 {
			final += "\n\nWarnings:\n- " + strings.Join(warnings, "\n- ")
		}
		return final, nil
	}

	return buildChunkSummaryFallback(results, warnings), nil
}

func buildChunkSummaryTasks(docs []*tools.ExtractedDocument) []chunkSummaryTask {
	tasks := make([]chunkSummaryTask, 0)
	index := 0
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		for _, chunk := range doc.Chunks {
			if strings.TrimSpace(chunk.Text) == "" {
				continue
			}
			tasks = append(tasks, chunkSummaryTask{
				index: index,
				path:  doc.SourcePath,
				chunk: chunk,
				doc:   doc,
			})
			index++
		}
	}
	return tasks
}

func buildChunkSummaryPrompt(path string, chunk tools.DocumentChunk, doc *tools.ExtractedDocument) string {
	warnings := chunkWarningLines(doc, chunk.PageStart, chunk.PageEnd)
	if len(warnings) == 0 {
		warnings = []string{"none"}
	}
	builder := strings.Builder{}
	builder.WriteString("You are Otter. Analyze this extracted document chunk for a local-first assistant using Qwen.\n")
	builder.WriteString("Return concise markdown with these sections exactly:\n")
	builder.WriteString("1. Summary\n2. Key Facts\n3. Open Questions\n\n")
	if chunk.Kind == "table_like" {
		builder.WriteString("If the chunk contains table-like content, include a short 'Tables' subsection under Key Facts.\n\n")
	}
	builder.WriteString("Source: ")
	builder.WriteString(path)
	builder.WriteString("\nPages: ")
	builder.WriteString(fmt.Sprintf("%d-%d", chunk.PageStart, chunk.PageEnd))
	builder.WriteString("\nContent type: ")
	builder.WriteString(chunk.Kind)
	builder.WriteString("\nWarnings: ")
	builder.WriteString(strings.Join(warnings, "; "))
	builder.WriteString("\n\nFocus on the most useful facts and avoid repeating the chunk verbatim.\n")
	builder.WriteString("Content:\n")
	builder.WriteString(chunk.Text)
	return builder.String()
}

func buildChunkMergePrompt(task string, docs []*tools.ExtractedDocument, results []chunkSummaryResult) string {
	builder := strings.Builder{}
	builder.WriteString("You are Otter. Merge chunk-level document analyses into one user-facing summary.\n")
	builder.WriteString("Return concise markdown with these sections exactly:\n")
	builder.WriteString("1. Summary\n2. Key Facts\n3. Open Questions\n\n")
	builder.WriteString("Keep it brief, deduplicate overlapping facts, and mention warnings only if they affect confidence.\n\n")
	builder.WriteString("User request: ")
	builder.WriteString(task)
	builder.WriteString("\n\nDocuments:\n")
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		builder.WriteString("- ")
		builder.WriteString(doc.SourcePath)
		builder.WriteString(" (")
		builder.WriteString(fmt.Sprintf("%d pages", doc.Metadata.PageCount))
		builder.WriteString(")\n")
	}
	builder.WriteString("\nChunk analyses:\n")
	for _, result := range results {
		builder.WriteString("### ")
		builder.WriteString(result.path)
		builder.WriteString(" pages ")
		builder.WriteString(fmt.Sprintf("%d-%d", result.chunk.PageStart, result.chunk.PageEnd))
		builder.WriteString("\n")
		if result.err != nil {
			builder.WriteString("FAILED: ")
			builder.WriteString(result.err.Error())
		} else {
			builder.WriteString(result.output)
		}
		builder.WriteString("\n\n")
	}
	return builder.String()
}

func buildChunkSummaryFallback(results []chunkSummaryResult, warnings []string) string {
	builder := strings.Builder{}
	builder.WriteString("Overview:\n")
	first := true
	for _, result := range results {
		if result.err != nil || strings.TrimSpace(result.output) == "" {
			continue
		}
		if !first {
			builder.WriteString("\n\n")
		}
		first = false
		builder.WriteString("### ")
		builder.WriteString(filepath.Base(result.path))
		builder.WriteString(" pages ")
		builder.WriteString(fmt.Sprintf("%d-%d", result.chunk.PageStart, result.chunk.PageEnd))
		builder.WriteString("\n")
		builder.WriteString(result.output)
	}
	if len(warnings) > 0 {
		builder.WriteString("\n\nWarnings:\n- ")
		builder.WriteString(strings.Join(warnings, "\n- "))
	}
	return strings.TrimSpace(builder.String())
}

func chunkProgressPreview(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(strings.TrimPrefix(line, "-"))
		if line == "" {
			continue
		}
		if len(line) > 140 {
			line = line[:140] + "..."
		}
		return line
	}
	return ""
}

func chunkWarningLines(doc *tools.ExtractedDocument, startPage, endPage int) []string {
	if doc == nil {
		return nil
	}
	lines := make([]string, 0)
	for _, warning := range doc.Warnings {
		if warning.PageNumber < startPage || warning.PageNumber > endPage {
			continue
		}
		lines = append(lines, fmt.Sprintf("page %d %s: %s", warning.PageNumber, warning.Code, warning.Message))
	}
	sort.Strings(lines)
	return lines
}
