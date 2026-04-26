package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	dslpdf "github.com/dslipak/pdf"
)

type PDFExtractor struct {
	opts ExtractOptions
}

var pdfLibraryNoiseMu sync.Mutex

func NewPDFExtractor(opts ExtractOptions) *PDFExtractor {
	return &PDFExtractor{opts: normalizeExtractOptions(opts)}
}

func (e *PDFExtractor) Extract(ctx context.Context, path string, info os.FileInfo) (*ExtractedDocument, error) {
	if info.Size() > e.opts.MaxFileSizeBytes {
		return nil, fmt.Errorf("pdf exceeds size limit: %s", path)
	}

	pdfKitDoc, pdfKitErr := extractWithPDFKit(ctx, path)
	if pdfKitErr != nil && !pdfKitAvailable() {
		pdfKitDoc = nil
	}
	if pdfKitDoc != nil && pdfKitDoc.Locked {
		return nil, fmt.Errorf("pdf is encrypted or locked: %s", path)
	}

	var reader *dslpdf.Reader
	var err error
	runWithSuppressedPDFNoise(func() {
		reader, err = dslpdf.Open(path)
	})
	if err != nil && pdfKitDoc == nil {
		return nil, fmt.Errorf("open pdf: %w", err)
	}

	totalPages := 0
	if pdfKitDoc != nil && pdfKitDoc.PageCount > totalPages {
		totalPages = pdfKitDoc.PageCount
	}
	if reader != nil && reader.NumPage() > totalPages {
		totalPages = reader.NumPage()
	}
	if totalPages == 0 {
		return nil, fmt.Errorf("pdf has no pages: %s", path)
	}
	if totalPages > e.opts.MaxPages {
		return nil, fmt.Errorf("pdf exceeds page limit (%d): %s", e.opts.MaxPages, path)
	}

	pages := make([]ExtractedPage, totalPages)
	warnings := make([]ExtractionWarning, 0, totalPages)
	type result struct {
		index    int
		page     ExtractedPage
		warnings []ExtractionWarning
	}

	jobs := make(chan int)
	results := make(chan result, totalPages)
	workers := e.opts.PageConcurrency
	for worker := 0; worker < workers; worker++ {
		go func() {
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				pageNum := index + 1
				var page dslpdf.Page
				if reader != nil && pageNum <= reader.NumPage() {
					page = reader.Page(pageNum)
				}
				var pdfKitPage *pdfKitPageResult
				if pdfKitDoc != nil && index < len(pdfKitDoc.Pages) {
					pdfKitPage = &pdfKitDoc.Pages[index]
				}
				extractedPage, pageWarnings := e.extractPage(ctx, path, pageNum, page, pdfKitPage)
				results <- result{index: index, page: extractedPage, warnings: pageWarnings}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for index := 0; index < totalPages; index++ {
			select {
			case <-ctx.Done():
				return
			case jobs <- index:
			}
		}
	}()

	for count := 0; count < totalPages; count++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case res := <-results:
			pages[res.index] = res.page
			warnings = append(warnings, res.warnings...)
		}
	}

	doc := &ExtractedDocument{
		SourcePath: path,
		FileType:   "pdf",
		Metadata: DocumentMetadata{
			FileSizeBytes: info.Size(),
			PageCount:     totalPages,
			ModifiedAt:    info.ModTime(),
		},
		Pages:    pages,
		Warnings: warnings,
	}
	e.populateMetadata(doc)
	doc.Chunks = buildDocumentChunks(doc, e.opts.ChunkTargetChars)
	if len(doc.Chunks) == 0 {
		return nil, fmt.Errorf("pdf has no extractable text: %s", path)
	}
	return doc, nil
}

func (e *PDFExtractor) extractPage(ctx context.Context, path string, pageNum int, page dslpdf.Page, pdfKitPage *pdfKitPageResult) (ExtractedPage, []ExtractionWarning) {
	extracted := ExtractedPage{
		PageNumber:     pageNum,
		Kind:           PageKindFailed,
		Confidence:     0.15,
		Method:         "none",
		FallbackReason: "",
	}
	if page.V.IsNull() && pdfKitPage == nil {
		warn := ExtractionWarning{Code: "page_missing", Message: "page is missing or unreadable", PageNumber: pageNum}
		extracted.Warnings = append(extracted.Warnings, warn)
		return extracted, []ExtractionWarning{warn}
	}

	pdfKitText := ""
	pdfKitScore := 0.0
	if pdfKitPage != nil {
		pdfKitText = normalizeExtractedPDFText(pdfKitPage.Text)
		pdfKitScore, _ = scoreExtractedText(pdfKitText)
	}

	tryGoNative := shouldTryGoNativePage(pdfKitText, pdfKitScore)
	var (
		content    dslpdf.Content
		rows       dslpdf.Rows
		plainText  string
		contentErr error
		rowErr     error
		plainErr   error
	)
	if !page.V.IsNull() && tryGoNative {
		content, contentErr = safePageContent(page)
		rows, rowErr = safePageRows(page)
		plainText, plainErr = safePagePlainText(page)
	}

	extracted.Blocks = blocksFromContent(content)
	rowLines := normalizeRows(rows)
	blockLines := linesFromBlocks(extracted.Blocks)
	rawBestText := pickBestPageText(
		pdfKitText,
		strings.TrimSpace(strings.Join(rowLines, "\n")),
		strings.TrimSpace(strings.Join(blockLines, "\n")),
		strings.TrimSpace(plainText),
		strings.TrimSpace(textFromBlocks(extracted.Blocks)),
	)
	table := buildExtractedTable(pdfKitLines(pdfKitText))
	if len(table.Rows) == 0 {
		table = buildExtractedTable(blockLines)
	}
	if len(table.Rows) == 0 {
		table = buildValueSequenceTable(rawBestText)
	}
	if len(table.Rows) > 0 {
		extracted.Tables = append(extracted.Tables, table)
	}
	extracted.Images = detectPageImages(page)

	goText := pickBestPageText(
		strings.TrimSpace(table.Markdown),
		strings.TrimSpace(strings.Join(rowLines, "\n")),
		strings.TrimSpace(strings.Join(blockLines, "\n")),
		strings.TrimSpace(plainText),
		strings.TrimSpace(textFromBlocks(extracted.Blocks)),
	)
	goScore, goReasons := scoreExtractedText(goText)
	pdfKitScore, pdfKitReasons := scoreExtractedText(pdfKitText)
	chosenText, chosenMethod, fallbackReason := choosePageText(pdfKitText, pdfKitScore, goText, goScore)
	extracted.Text = chosenText
	extracted.Method = chosenMethod
	extracted.FallbackReason = fallbackReason
	extracted.CharCount = len([]rune(strings.TrimSpace(chosenText)))

	if contentErr != nil {
		extracted.Warnings = append(extracted.Warnings, ExtractionWarning{Code: "content_parse_failed", Message: contentErr.Error(), PageNumber: pageNum})
	}
	if rowErr != nil {
		extracted.Warnings = append(extracted.Warnings, ExtractionWarning{Code: "row_parse_failed", Message: rowErr.Error(), PageNumber: pageNum})
	}
	if plainErr != nil {
		extracted.Warnings = append(extracted.Warnings, ExtractionWarning{Code: "plain_text_parse_failed", Message: plainErr.Error(), PageNumber: pageNum})
	}
	if chosenMethod == "pdfkit" && len(pdfKitReasons) > 0 {
		extracted.Warnings = append(extracted.Warnings, ExtractionWarning{Code: "pdfkit_quality_notes", Message: strings.Join(pdfKitReasons, "; "), PageNumber: pageNum})
	}
	if chosenMethod != "pdfkit" && len(goReasons) > 0 {
		extracted.Warnings = append(extracted.Warnings, ExtractionWarning{Code: "native_quality_notes", Message: strings.Join(goReasons, "; "), PageNumber: pageNum})
	}

	if extracted.Text == "" && len(extracted.Images) > 0 {
		ocrResult, err := e.opts.OCR.OCRPage(ctx, OCRPageRequest{
			SourcePath: path,
			PageNumber: pageNum,
			PageKind:   PageKindImageOnly,
		})
		if err != nil {
			extracted.Warnings = append(extracted.Warnings, ExtractionWarning{
				Code:       "ocr_unavailable",
				Message:    err.Error(),
				PageNumber: pageNum,
			})
		} else {
			extracted.OCRText = strings.TrimSpace(ocrResult.Text)
			extracted.Confidence = maxFloat(extracted.Confidence, ocrResult.Confidence)
			extracted.Warnings = append(extracted.Warnings, ocrResult.Warnings...)
		}
	}

	extracted.Kind = classifyPage(extracted, len(content.Rect), rowErr != nil || plainErr != nil)
	extracted.Confidence = pageConfidence(extracted, extracted.Method, pdfKitScore, goScore)
	if strings.TrimSpace(extracted.Text) == "" && strings.TrimSpace(extracted.OCRText) != "" {
		extracted.Text = extracted.OCRText
	}
	if extracted.Kind == PageKindImageOnly && strings.TrimSpace(extracted.Text) == "" {
		extracted.Warnings = append(extracted.Warnings, ExtractionWarning{
			Code:       "image_page_without_ocr",
			Message:    "page appears image-based and OCR did not produce text",
			PageNumber: pageNum,
		})
	}
	if extracted.Kind == PageKindFailed {
		extracted.Warnings = append(extracted.Warnings, ExtractionWarning{
			Code:       "page_failed",
			Message:    "page extraction produced no usable content",
			PageNumber: pageNum,
		})
	}
	return extracted, extracted.Warnings
}

func (e *PDFExtractor) populateMetadata(doc *ExtractedDocument) {
	for _, page := range doc.Pages {
		switch page.Kind {
		case PageKindTextNative:
			doc.Metadata.NativePages++
		case PageKindTableLike:
			doc.Metadata.TablePages++
		case PageKindImageOnly:
			doc.Metadata.ImagePages++
		case PageKindMixed:
			doc.Metadata.MixedPages++
		case PageKindFailed:
			doc.Metadata.FailedPages++
		}
		if strings.TrimSpace(page.OCRText) != "" {
			doc.Metadata.OCRPages++
		}
	}
}

func safePageContent(page dslpdf.Page) (dslpdf.Content, error) {
	var content dslpdf.Content
	var err error
	defer func() {
		if recovered := recover(); recovered != nil {
			content = dslpdf.Content{}
			err = fmt.Errorf("%v", recovered)
		}
	}()
	runWithSuppressedPDFNoise(func() {
		content = page.Content()
	})
	return content, err
}

func safePageRows(page dslpdf.Page) (dslpdf.Rows, error) {
	var (
		rows dslpdf.Rows
		err  error
	)
	runWithSuppressedPDFNoise(func() {
		rows, err = page.GetTextByRow()
	})
	return rows, err
}

func safePagePlainText(page dslpdf.Page) (string, error) {
	var (
		text string
		err  error
	)
	runWithSuppressedPDFNoise(func() {
		text, err = page.GetPlainText(nil)
	})
	return text, err
}

func blocksFromContent(content dslpdf.Content) []TextBlock {
	blocks := make([]TextBlock, 0, len(content.Text))
	for _, text := range content.Text {
		clean := strings.TrimSpace(text.S)
		if clean == "" {
			continue
		}
		blocks = append(blocks, TextBlock{
			Text:     clean,
			X:        text.X,
			Y:        text.Y,
			Width:    text.W,
			Height:   text.FontSize,
			Font:     text.Font,
			FontSize: text.FontSize,
		})
	}
	sort.SliceStable(blocks, func(i, j int) bool {
		if blocks[i].Y == blocks[j].Y {
			return blocks[i].X < blocks[j].X
		}
		return blocks[i].Y > blocks[j].Y
	})
	return blocks
}

func textFromBlocks(blocks []TextBlock) string {
	return strings.TrimSpace(strings.Join(linesFromBlocks(blocks), "\n"))
}

func linesFromBlocks(blocks []TextBlock) []string {
	if len(blocks) == 0 {
		return nil
	}
	lines := make([]string, 0, len(blocks))
	var currentY float64
	var currentLine strings.Builder
	var prevRight float64
	var prevFontSize float64
	for index, block := range blocks {
		if index == 0 {
			currentY = block.Y
			prevRight = block.X + block.Width
			prevFontSize = block.FontSize
			currentLine.WriteString(strings.TrimSpace(block.Text))
			continue
		}
		if absFloat(currentY-block.Y) > 3 {
			lines = append(lines, currentLine.String())
			currentLine.Reset()
			currentY = block.Y
			currentLine.WriteString(strings.TrimSpace(block.Text))
			prevRight = block.X + block.Width
			prevFontSize = block.FontSize
			continue
		}

		gap := block.X - prevRight
		spaceThreshold := prevFontSize * 0.35
		if spaceThreshold < 1.5 {
			spaceThreshold = 1.5
		}
		if gap > spaceThreshold && currentLine.Len() > 0 {
			currentLine.WriteString(" ")
		}
		currentLine.WriteString(strings.TrimSpace(block.Text))
		prevRight = block.X + block.Width
		prevFontSize = block.FontSize
	}
	if currentLine.Len() > 0 {
		lines = append(lines, currentLine.String())
	}
	return lines
}

func normalizeRows(rows dslpdf.Rows) []string {
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		parts := make([]string, 0, len(row.Content))
		for _, item := range row.Content {
			part := strings.TrimSpace(item.S)
			if part != "" {
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 {
			continue
		}
		line := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
		lines = append(lines, normalizeExtractedPDFText(line))
	}
	return lines
}

func buildExtractedTable(lines []string) ExtractedTable {
	rows := make([][]string, 0, len(lines))
	multiColumnRows := 0
	for _, line := range lines {
		cells := splitTableLine(line)
		if len(cells) >= 2 {
			multiColumnRows++
		}
		rows = append(rows, cells)
	}
	if multiColumnRows < 2 {
		return ExtractedTable{}
	}
	return ExtractedTable{
		Rows:       rows,
		Markdown:   renderMarkdownTable(rows),
		Confidence: 0.72,
	}
}

func buildValueSequenceTable(text string) ExtractedTable {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) < 4 {
		return ExtractedTable{}
	}
	rows := make([][]string, 0)
	label := make([]string, 0)
	for _, field := range fields {
		if looksNumeric(field) {
			if len(label) == 0 {
				continue
			}
			if len(rows) == 0 && len(label) >= 3 {
				label = label[1:]
			}
			rows = append(rows, []string{strings.Join(label, " "), field})
			label = nil
			continue
		}
		label = append(label, field)
	}
	if len(rows) < 2 {
		return ExtractedTable{}
	}
	return ExtractedTable{
		Rows:       rows,
		Markdown:   renderMarkdownTable(rows),
		Confidence: 0.58,
	}
}

func splitTableLine(line string) []string {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var cells []string
	switch {
	case strings.Contains(line, "  "):
		for _, part := range strings.Split(line, "  ") {
			part = strings.TrimSpace(part)
			if part != "" {
				cells = append(cells, part)
			}
		}
	case len(strings.Fields(line)) >= 4:
		fields := strings.Fields(line)
		mid := len(fields) / 2
		cells = append(cells, strings.Join(fields[:mid], " "), strings.Join(fields[mid:], " "))
	case len(strings.Fields(line)) >= 2 && looksNumeric(strings.Fields(line)[len(strings.Fields(line))-1]):
		fields := strings.Fields(line)
		cells = append(cells, strings.Join(fields[:len(fields)-1], " "), fields[len(fields)-1])
	default:
		cells = append(cells, line)
	}
	if len(cells) == 0 {
		return []string{line}
	}
	return cells
}

func renderMarkdownTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	maxCols := 0
	for _, row := range rows {
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}
	if maxCols < 2 {
		return ""
	}
	normalized := make([][]string, 0, len(rows))
	for _, row := range rows {
		current := append([]string(nil), row...)
		for len(current) < maxCols {
			current = append(current, "")
		}
		normalized = append(normalized, current)
	}
	var buf bytes.Buffer
	writeTableRow := func(row []string) {
		buf.WriteString("| ")
		buf.WriteString(strings.Join(row, " | "))
		buf.WriteString(" |\n")
	}
	writeTableRow(normalized[0])
	divider := make([]string, maxCols)
	for i := range divider {
		divider[i] = "---"
	}
	writeTableRow(divider)
	for _, row := range normalized[1:] {
		writeTableRow(row)
	}
	return strings.TrimSpace(buf.String())
}

func detectPageImages(page dslpdf.Page) []ExtractedImage {
	xobjects := page.Resources().Key("XObject")
	keys := xobjects.Keys()
	images := make([]ExtractedImage, 0, len(keys))
	for _, key := range keys {
		obj := xobjects.Key(key)
		if strings.EqualFold(obj.Key("Subtype").Name(), "Image") {
			images = append(images, ExtractedImage{
				Index:      len(images) + 1,
				Width:      int(obj.Key("Width").Int64()),
				Height:     int(obj.Key("Height").Int64()),
				AltSummary: "Image object detected but not interpreted.",
			})
		}
	}
	return images
}

func pickBestPageText(candidates ...string) string {
	best := ""
	for _, candidate := range candidates {
		candidate = normalizeExtractedPDFText(sanitizeExtractedText(candidate))
		candidate = strings.TrimSpace(candidate)
		if len(candidate) > len(best) {
			best = candidate
		}
	}
	if len(best) > 16000 {
		best = best[:16000]
	}
	return best
}

func classifyPage(page ExtractedPage, rectCount int, hadParseFailure bool) PageKind {
	text := strings.TrimSpace(page.Text)
	hasText := text != ""
	hasImages := len(page.Images) > 0
	hasTable := len(page.Tables) > 0
	numericTailRows := countNumericTrailingRows(text)

	switch {
	case !hasText && !hasImages && hadParseFailure:
		return PageKindFailed
	case !hasText && hasImages:
		return PageKindImageOnly
	case !hasText && !hasImages:
		return PageKindEmpty
	case hasImages && hasText:
		if hasTable {
			return PageKindMixed
		}
		return PageKindMixed
	case hasTable:
		return PageKindTableLike
	case numericTailRows >= 2 && lineCount(text) >= 3:
		return PageKindTableLike
	case rectCount >= 6 && lineCount(text) >= 3:
		return PageKindTableLike
	default:
		return PageKindTextNative
	}
}

func pageConfidence(page ExtractedPage, method string, pdfKitScore, goScore float64) float64 {
	confidence := pdfKitScore
	if method != "pdfkit" {
		confidence = goScore
	}
	if strings.TrimSpace(page.OCRText) != "" && confidence < 0.6 {
		confidence = 0.6
	}
	if len(page.Tables) > 0 && confidence < 0.72 {
		confidence = 0.72
	}
	if confidence < 0.15 {
		confidence = 0.15
	}
	if confidence > 0.99 {
		confidence = 0.99
	}
	return confidence
}

func buildDocumentChunks(doc *ExtractedDocument, targetChars int) []DocumentChunk {
	if doc == nil || len(doc.Pages) == 0 {
		return nil
	}
	if targetChars <= 0 {
		targetChars = 3200
	}

	var chunks []DocumentChunk
	var current []string
	pageStart := 0
	pageEnd := 0
	pageKinds := make(map[PageKind]int)
	currentLen := 0

	flush := func() {
		if len(current) == 0 || pageStart == 0 {
			return
		}
		text := strings.TrimSpace(strings.Join(current, "\n\n"))
		if text == "" {
			current = nil
			currentLen = 0
			pageStart = 0
			pageKinds = make(map[PageKind]int)
			return
		}
		chunkID := "chunk-" + strconv.Itoa(len(chunks)+1)
		chunks = append(chunks, DocumentChunk{
			ID:            chunkID,
			PageStart:     pageStart,
			PageEnd:       pageEnd,
			Text:          text,
			Kind:          dominantPageKind(pageKinds),
			TokenEstimate: estimateTokenCount(text),
			Metadata: map[string]string{
				"source_path": doc.SourcePath,
				"file_type":   doc.FileType,
				"page_range":  fmt.Sprintf("%d-%d", pageStart, pageEnd),
			},
		})
		current = nil
		currentLen = 0
		pageStart = 0
		pageEnd = 0
		pageKinds = make(map[PageKind]int)
	}

	for _, page := range doc.Pages {
		pageText := strings.TrimSpace(renderPageForChunk(page))
		if pageText == "" {
			continue
		}
		if pageStart == 0 {
			pageStart = page.PageNumber
		}
		if currentLen > 0 && currentLen+len(pageText) > targetChars {
			flush()
			pageStart = page.PageNumber
		}
		current = append(current, pageText)
		currentLen += len(pageText)
		pageEnd = page.PageNumber
		pageKinds[page.Kind]++
	}
	if len(current) > 0 {
		flush()
	}
	return chunks
}

func renderPageForChunk(page ExtractedPage) string {
	text := strings.TrimSpace(page.Text)
	if text == "" {
		text = strings.TrimSpace(page.OCRText)
	}
	if text == "" {
		return ""
	}
	text = stripLowValuePageLines(text)
	var builder strings.Builder
	if page.FallbackReason != "" {
		builder.WriteString("Extraction note: ")
		builder.WriteString(page.FallbackReason)
		builder.WriteString("\n")
	}
	builder.WriteString(text)
	if markdown := chooseTableMarkdownForChunk(text, page.Tables); markdown != "" {
		builder.WriteString("\n\nTable:\n")
		builder.WriteString(markdown)
	}
	if len(page.Warnings) > 0 {
		builder.WriteString("\n\nWarnings:\n")
		for _, warning := range page.Warnings {
			builder.WriteString("- ")
			builder.WriteString(warning.Code)
			builder.WriteString(": ")
			builder.WriteString(warning.Message)
			builder.WriteString("\n")
		}
	}
	return strings.TrimSpace(builder.String())
}

func stripLowValuePageLines(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			filtered = append(filtered, "")
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "page ") && strings.Contains(lower, " of ") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

func chooseTableMarkdownForChunk(text string, tables []ExtractedTable) string {
	if len(tables) == 0 {
		return ""
	}
	markdown := strings.TrimSpace(tables[0].Markdown)
	if markdown == "" {
		return ""
	}
	if tableMostlyDuplicatesText(text, markdown) {
		return ""
	}
	return markdown
}

func tableMostlyDuplicatesText(text, markdown string) bool {
	textTokens := tokenSetForOverlap(text)
	tableTokens := tokenSetForOverlap(markdown)
	if len(textTokens) == 0 || len(tableTokens) == 0 {
		return false
	}
	shared := 0
	for token := range tableTokens {
		if _, ok := textTokens[token]; ok {
			shared++
		}
	}
	return shared*100/len(tableTokens) >= 70
}

func tokenSetForOverlap(text string) map[string]struct{} {
	replacer := strings.NewReplacer(
		"|", " ",
		"\n", " ",
		"\t", " ",
		",", " ",
		".", " ",
		":", " ",
		";", " ",
		"(", " ",
		")", " ",
		"[", " ",
		"]", " ",
	)
	normalized := strings.ToLower(replacer.Replace(text))
	fields := strings.Fields(normalized)
	tokens := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if len(field) < 3 {
			continue
		}
		tokens[field] = struct{}{}
	}
	return tokens
}

func normalizeExtractedPDFText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = collapseCharacterSpacedLine(strings.TrimSpace(line))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func collapseCharacterSpacedLine(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return line
	}
	singleChar := 0
	for _, field := range fields {
		if len([]rune(field)) == 1 && field != "-" {
			singleChar++
		}
	}
	if singleChar*100/len(fields) < 70 {
		return line
	}

	var builder strings.Builder
	for i, field := range fields {
		if i == 0 {
			builder.WriteString(field)
			continue
		}
		prev := fields[i-1]
		if len([]rune(prev)) == 1 && len([]rune(field)) == 1 {
			builder.WriteString(field)
			continue
		}
		builder.WriteString(" ")
		builder.WriteString(field)
	}
	return builder.String()
}

func looksNumeric(value string) bool {
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "$"), "%"))
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && r != '.' && r != ',' && r != '-' {
			return false
		}
	}
	return true
}

func countNumericTrailingRows(text string) int {
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if looksNumeric(fields[len(fields)-1]) {
			count++
		}
	}
	return count
}

func dominantPageKind(kinds map[PageKind]int) string {
	type pair struct {
		kind  PageKind
		count int
	}
	top := pair{kind: PageKindTextNative}
	for kind, count := range kinds {
		if count > top.count {
			top = pair{kind: kind, count: count}
		}
	}
	return string(top.kind)
}

func lineCount(text string) int {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0
	}
	return 1 + strings.Count(trimmed, "\n")
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func runWithSuppressedPDFNoise(run func()) {
	pdfLibraryNoiseMu.Lock()
	defer pdfLibraryNoiseMu.Unlock()

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		run()
		return
	}
	defer devNull.Close()
	dupFD, err := syscall.Dup(2)
	if err != nil {
		run()
		return
	}
	defer syscall.Close(dupFD)
	if err := syscall.Dup2(int(devNull.Fd()), 2); err != nil {
		run()
		return
	}
	defer syscall.Dup2(dupFD, 2)
	run()
}

func pdfKitLines(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lines := strings.Split(normalizeExtractedPDFText(text), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func scoreExtractedText(text string) (float64, []string) {
	text = normalizeExtractedPDFText(text)
	if strings.TrimSpace(text) == "" {
		return 0, []string{"empty text"}
	}
	chars := len([]rune(text))
	lines := pdfKitLines(text)
	tokens := strings.Fields(text)
	if len(tokens) == 0 {
		return 0.05, []string{"no tokens"}
	}
	meaningful := 0
	singleChar := 0
	for _, token := range tokens {
		if len([]rune(token)) == 1 {
			singleChar++
		}
		if tokenLooksMeaningful(token) {
			meaningful++
		}
	}
	meaningfulRatio := float64(meaningful) / float64(len(tokens))
	singleCharRatio := float64(singleChar) / float64(len(tokens))
	duplicateRatio := duplicateLineRatio(lines)

	score := minFloat(1, float64(chars)/1200.0)*0.35 + meaningfulRatio*0.45 + (1-duplicateRatio)*0.20
	reasons := make([]string, 0, 4)
	if chars < 80 {
		reasons = append(reasons, "low character count")
		score -= 0.15
	}
	if meaningfulRatio < 0.55 {
		reasons = append(reasons, "low meaningful token ratio")
		score -= 0.20
	}
	if singleCharRatio > 0.35 {
		reasons = append(reasons, "excessive single-character tokens")
		score -= 0.25
	}
	if duplicateRatio > 0.35 {
		reasons = append(reasons, "duplicate line repetition")
		score -= 0.10
	}
	if strings.Contains(strings.ToLower(text), "interp dup") {
		reasons = append(reasons, "parser artifact detected")
		score -= 0.40
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return score, reasons
}

func choosePageText(pdfKitText string, pdfKitScore float64, goText string, goScore float64) (string, string, string) {
	pdfKitText = strings.TrimSpace(pdfKitText)
	goText = strings.TrimSpace(goText)
	switch {
	case pdfKitText != "" && (goText == "" || pdfKitScore >= goScore+0.05):
		return pdfKitText, "pdfkit", ""
	case goText != "" && (pdfKitText == "" || goScore > pdfKitScore):
		reason := ""
		if pdfKitText != "" {
			reason = "replaced lower-quality pdfkit extraction"
		}
		return goText, "dslipak", reason
	case pdfKitText != "":
		return pdfKitText, "pdfkit", ""
	case goText != "":
		return goText, "dslipak", ""
	default:
		return "", "none", ""
	}
}

func shouldTryGoNativePage(pdfKitText string, pdfKitScore float64) bool {
	if strings.TrimSpace(pdfKitText) == "" {
		return true
	}
	if pdfKitScore < 0.62 {
		return true
	}
	if countNumericTrailingRows(pdfKitText) >= 3 {
		return true
	}
	return false
}

func tokenLooksMeaningful(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	hasAlphaNum := false
	for _, r := range token {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			hasAlphaNum = true
			break
		}
	}
	return hasAlphaNum
}

func duplicateLineRatio(lines []string) float64 {
	if len(lines) <= 1 {
		return 0
	}
	seen := make(map[string]int, len(lines))
	duplicates := 0
	for _, line := range lines {
		normalized := strings.ToLower(strings.TrimSpace(line))
		if normalized == "" {
			continue
		}
		seen[normalized]++
		if seen[normalized] > 1 {
			duplicates++
		}
	}
	return float64(duplicates) / float64(len(lines))
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
