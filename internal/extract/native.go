package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode/utf8"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/source"
	htmltoken "golang.org/x/net/html"
)

const (
	maxNativeInput  = 256 << 20
	maxNativeOutput = 16 << 20
	maxPDFPages     = 5000
	maxImagePixels  = 40_000_000
	// maxOCRPixels bounds the total raster area OCR'd for one PDF (about 57
	// letter pages at 150 dpi); pages with a text layer do not count.
	maxOCRPixels = 120_000_000
	// pdfTextBatchPages is how many pages one pdftotext run extracts, so the
	// per-process CPU and output limits bound a batch rather than the document.
	pdfTextBatchPages = 50
)

type NativeCapabilities struct {
	PDF       bool
	OCR       bool
	Languages []string
	Reason    string
}

// ProbeNative reports only capabilities available inside the required filesystem and network sandbox.
func ProbeNative() NativeCapabilities {
	if nativeWorkerPath() != "" {
		return probeNativeWorker()
	}
	nativeProbe.Do(func() { nativeCaps = probeNativeLocally() })
	caps := nativeCaps
	caps.Languages = append([]string(nil), nativeCaps.Languages...)
	return caps
}

func probeNativeLocally() NativeCapabilities {
	var caps NativeCapabilities
	sandbox, e1 := exec.LookPath("bwrap")
	prlimit, e2 := exec.LookPath("prlimit")
	if e1 != nil || e2 != nil {
		if nativeSandboxMode() != nativeSandboxContainer {
			caps.Reason = "prlimit and bubblewrap are required for native processing"
			return caps
		}
	} else if nativeSandboxMode() == nativeSandboxContainer {
		sandbox = ""
	}
	// Probe with prlimit itself: it is always present, and unlike `true` it
	// lives under /usr on every distribution (Alpine has only /bin/true).
	if _, err := runNative(context.Background(), sandbox, prlimit, prlimit, []string{"--version"}, "", 4096); err != nil {
		caps.Reason = "native processor sandbox unavailable: " + err.Error()
		return caps
	}
	_, textErr := exec.LookPath("pdftotext")
	_, rasterErr := exec.LookPath("pdftoppm")
	_, infoErr := exec.LookPath("pdfinfo")
	if textErr == nil && rasterErr == nil && infoErr == nil {
		caps.PDF = true
	} else {
		caps.Reason = "Poppler pdfinfo, pdftotext and pdftoppm are required"
	}
	if tesseract, err := exec.LookPath("tesseract"); err == nil {
		caps.Languages = availableLanguages(tesseract, sandbox, prlimit)
		caps.OCR = len(caps.Languages) > 0
	}
	if !caps.OCR {
		if caps.Reason != "" {
			caps.Reason += "; "
		}
		caps.Reason += "Tesseract or OCR language assets are unavailable"
	}
	return caps
}

var nativeProbe sync.Once
var nativeCaps NativeCapabilities

// nativeSandbox selects how runNative isolates tools: "bwrap" wraps them in
// bubblewrap + prlimit (the default), "container" runs prlimit directly —
// only the extract-worker subcommand may select it, since there the process
// itself runs inside the sandboxed sidecar container.
var nativeSandbox atomic.Int32

const (
	nativeSandboxBwrap     int32 = 0
	nativeSandboxContainer int32 = 1
)

func nativeSandboxMode() int32 { return nativeSandbox.Load() }

func enableContainerSandbox() { nativeSandbox.Store(nativeSandboxContainer) }

type limitedBuffer struct {
	b     bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.limit = 64 << 10
	}
	n := len(p)
	remaining := b.limit - b.b.Len()
	if remaining > 0 {
		if n > remaining {
			_, _ = b.b.Write(p[:remaining])
		} else {
			_, _ = b.b.Write(p)
		}
	}
	return n, nil
}

func (b *limitedBuffer) String() string { return b.b.String() }

func runNativeInContainer(ctx context.Context, prlimit, binary string, args []string, dir string, maxOutput int64) ([]byte, error) {
	// No --nproc here: without a user namespace that limit counts every
	// process of the uid on the host (the app container shares it), so
	// Tesseract's worker threads failed to start. The container's pids limit
	// bounds processes instead, and Tesseract is held to one thread.
	cmd := exec.CommandContext(ctx, prlimit, append([]string{"--cpu=30", "--as=536870912", "--fsize=16777216", "--", binary}, args...)...) //nolint:gosec // prlimit, binary and args are server-resolved PATH lookups of fixed tool names; args are process-confined
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=/tmp", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "LD_LIBRARY_PATH=/usr/lib:/usr/lib64", "OMP_THREAD_LIMIT=1"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr limitedBuffer
	stderr.limit = 64 << 10
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	stopKill := context.AfterFunc(ctx, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxOutput+1))
	if readErr != nil || len(out) > int(maxOutput) {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	waitErr := cmd.Wait()
	stopKill()
	if readErr != nil {
		return nil, readErr
	}
	if len(out) > int(maxOutput) {
		return nil, ErrDocumentTooLarge
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if waitErr != nil {
		return nil, fmt.Errorf("native processor failed: %s", strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func availableLanguages(bin, sandbox, prlimit string) []string {
	out, err := runNative(context.Background(), sandbox, prlimit, bin, []string{"--list-langs"}, "", 2<<20)
	if err != nil {
		return nil
	}
	var langs []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.Contains(line, "available languages") {
			langs = append(langs, line)
		}
	}
	return langs
}

func runNative(ctx context.Context, sandbox, prlimit, binary string, args []string, dir string, maxOutput int64) ([]byte, error) {
	if maxOutput <= 0 || maxOutput > maxNativeOutput {
		maxOutput = maxNativeOutput
	}
	cleanup := func() {}
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "enclava-native-")
		if err != nil {
			return nil, err
		}
		cleanup = func() { _ = os.RemoveAll(dir) }
		if err := os.Chmod(dir, 0700); err != nil { //nolint:gosec // fresh MkdirTemp dir: 0700 is minimal for a private directory (needs the execute bit)
			cleanup()
			return nil, err
		}
	}
	defer cleanup()
	if nativeSandboxMode() == nativeSandboxContainer {
		if prlimit == "" {
			return nil, errors.New("prlimit is required for native processing")
		}
		rewritten := make([]string, len(args))
		for i, arg := range args {
			if strings.HasPrefix(arg, "/work") {
				arg = dir + strings.TrimPrefix(arg, "/work")
			}
			rewritten[i] = arg
		}
		return runNativeInContainer(ctx, prlimit, binary, rewritten, dir, maxOutput)
	}
	cmdArgs := make([]string, 0, 31+len(args)) // 31 fixed sandbox/prlimit arguments below
	cmdArgs = append(cmdArgs, "--die-with-parent", "--new-session", "--unshare-all", "--ro-bind", "/usr", "/usr", "--ro-bind", "/lib", "/lib", "--ro-bind", "/lib64", "/lib64", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--bind", dir, "/work", "--chdir", "/work", "--", prlimit, "--cpu=30", "--as=536870912", "--nproc=16", "--fsize=16777216", "--", binary)
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, sandbox, cmdArgs...) //nolint:gosec // sandbox, prlimit and binary are server-resolved PATH lookups of fixed tool names; args are sandbox-confined
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/work", "TMPDIR=/tmp", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "LD_LIBRARY_PATH=/usr/lib:/usr/lib64"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr limitedBuffer
	stderr.limit = 64 << 10
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	stopKill := context.AfterFunc(ctx, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxOutput+1))
	if readErr != nil || len(out) > int(maxOutput) {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	waitErr := cmd.Wait()
	stopKill()
	if readErr != nil {
		return nil, readErr
	}
	if len(out) > int(maxOutput) {
		return nil, ErrDocumentTooLarge
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if waitErr != nil {
		return nil, fmt.Errorf("native processor failed: %s", strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func parseNative(ctx context.Context, ref artifacts.Ref, opts ParseOptions, data []byte, format string) (source.Document, error) {
	if nativeWorkerPath() != "" {
		return parseNativeViaWorker(ctx, ref, opts, data, format)
	}
	blocks, warnings, err := parseNativeBlocks(ctx, data, format)
	if err != nil {
		return source.Document{}, err
	}
	document := makeDocument(opts.ObjectRef, ref, blocks)
	document.ParseWarnings = warnings
	return document, nil
}

// parseNativeBlocks runs the local native pipeline and returns raw blocks and
// warnings; parseNative wraps them into a source.Document. The extract-worker
// HTTP handler serves exactly this result set.
func parseNativeBlocks(ctx context.Context, data []byte, format string) ([]parseBlock, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if len(data) > maxNativeInput {
		return nil, nil, fmt.Errorf("%w: file is %d MB, the limit is %d MB", ErrDocumentTooLarge, len(data)>>20, maxNativeInput>>20)
	}
	if format == "image" {
		if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
			return nil, nil, fmt.Errorf("%w: %s is unsupported; only PNG image OCR is supported", ErrUnsupportedFormat, http.DetectContentType(data))
		}
		pixels, err := imageDimensions(data)
		if err != nil {
			return nil, nil, err
		}
		if pixels > maxImagePixels {
			return nil, nil, ErrDocumentTooLarge
		}
	}
	caps := ProbeNative()
	if format == "pdf" && !caps.PDF {
		return nil, nil, fmt.Errorf("extract: PDF unavailable: %s", caps.Reason)
	}
	if format == "image" && !caps.OCR {
		return nil, nil, fmt.Errorf("extract: OCR unavailable: %s", caps.Reason)
	}
	sandbox, _ := exec.LookPath("bwrap")
	prlimit, _ := exec.LookPath("prlimit")
	dir, err := os.MkdirTemp("", "enclava-native-")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err = os.Chmod(dir, 0700); err != nil { //nolint:gosec // fresh MkdirTemp dir: 0700 is minimal for a private directory (needs the execute bit)
		return nil, nil, err
	}
	if err = os.WriteFile(filepath.Join(dir, "input"), data, 0600); err != nil { //nolint:gosec // G703: dir is a fresh private MkdirTemp dir; the path is not attacker-controlled
		return nil, nil, err
	}
	var blocks []parseBlock
	var parseWarnings []string
	if format == "pdf" {
		pdfinfo, _ := exec.LookPath("pdfinfo")
		info, err := runNative(ctx, sandbox, prlimit, pdfinfo, []string{"/work/input"}, dir, 1<<20)
		if err != nil {
			if encryptedPDF(info, err) {
				return nil, nil, ErrEncryptedDocument
			}
			return nil, nil, fmt.Errorf("%w: PDF metadata: %v", ErrMalformedDocument, err)
		}
		pages, err := pdfPageCount(info)
		if err != nil {
			return nil, nil, err
		}
		if pages > maxPDFPages {
			return nil, nil, fmt.Errorf("%w: PDF has %d pages, the limit is %d", ErrDocumentTooLarge, pages, maxPDFPages)
		}
		pdftotext, _ := exec.LookPath("pdftotext")
		for first := 1; first <= pages; first += pdfTextBatchPages {
			last := min(first+pdfTextBatchPages-1, pages)
			bbox, err := runNative(ctx, sandbox, prlimit, pdftotext, []string{"-bbox", "-f", strconv.Itoa(first), "-l", strconv.Itoa(last), "/work/input", "-"}, dir, maxNativeOutput)
			if err != nil {
				if encryptedPDF(info, err) {
					return nil, nil, ErrEncryptedDocument
				}
				if errors.Is(err, ErrDocumentTooLarge) {
					return nil, nil, fmt.Errorf("%w: text of PDF pages %d-%d exceeds %d MB", ErrDocumentTooLarge, first, last, maxNativeOutput>>20)
				}
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				return nil, nil, fmt.Errorf("%w: PDF text: %v", ErrMalformedDocument, err)
			}
			batch, err := parsePDFBBox(bbox, first)
			if err != nil {
				return nil, nil, err
			}
			blocks = append(blocks, batch...)
		}
		hasText := make(map[int]bool)
		for _, block := range blocks {
			if block.locator.Page != nil {
				hasText[*block.locator.Page] = true
			}
		}
		var rasterPixels int
		if !caps.OCR && len(hasText) < pages {
			return nil, nil, fmt.Errorf("extract: PDF OCR unavailable: %s", caps.Reason)
		}
		for page := 1; page <= pages; page++ {
			if hasText[page] {
				parseWarnings = append(parseWarnings, fmt.Sprintf("page %d contains embedded text; scanned regions not OCR'd", page))
			}
		}
		if caps.OCR {
			for page := 1; page <= pages; page++ {
				if hasText[page] {
					continue
				}
				pageBlocks, err := ocrPDFPage(ctx, sandbox, prlimit, dir, page, ocrLanguage(caps.Languages), &rasterPixels)
				if err != nil {
					return nil, nil, err
				}
				blocks = append(blocks, pageBlocks...)
			}
		}
		sort.SliceStable(blocks, func(i, j int) bool {
			pi, pj := 0, 0
			if blocks[i].locator.Page != nil {
				pi = *blocks[i].locator.Page
			}
			if blocks[j].locator.Page != nil {
				pj = *blocks[j].locator.Page
			}
			return pi < pj
		})
		offset := 0
		for i := range blocks {
			blocks[i].start, blocks[i].end = offset, offset+len(blocks[i].text)
			offset = blocks[i].end + 1
		}
		if len(blocks) == 0 {
			return nil, nil, fmt.Errorf("%w: PDF contains no extractable text", ErrMalformedDocument)
		}
	} else {
		tesseract, _ := exec.LookPath("tesseract")
		raw, err := runNative(ctx, sandbox, prlimit, tesseract, []string{"/work/input", "stdout", "-l", ocrLanguage(caps.Languages), "tsv"}, dir, maxNativeOutput)
		if err != nil {
			return nil, nil, err
		}
		blocks, err = parseOCRTSV(raw)
		if err != nil {
			return nil, nil, err
		}
	}
	return blocks, parseWarnings, nil
}

func encryptedPDF(info []byte, err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "incorrect password") || strings.Contains(message, "incorrect owner password") {
		return true
	}
	for _, line := range strings.Split(string(info), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "Encrypted") && strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "yes") {
			return true
		}
	}
	return false
}

func pdfPageCount(info []byte) (int, error) {
	for _, line := range strings.Split(string(info), "\n") {
		if name, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == "Pages" {
			pages, err := strconv.Atoi(strings.TrimSpace(value))
			if err == nil && pages > 0 {
				return pages, nil
			}
		}
	}
	return 0, fmt.Errorf("%w: missing PDF page count", ErrMalformedDocument)
}

func ocrLanguage(languages []string) string {
	for _, language := range languages {
		if language == "eng" {
			return language
		}
	}
	if len(languages) > 0 {
		return languages[0]
	}
	return ""
}

func ocrPDFPage(ctx context.Context, sandbox, prlimit, dir string, page int, language string, totalPixels *int) ([]parseBlock, error) {
	pdftoppm, _ := exec.LookPath("pdftoppm")
	if _, err := runNative(ctx, sandbox, prlimit, pdftoppm, []string{"-f", strconv.Itoa(page), "-l", strconv.Itoa(page), "-r", "150", "-singlefile", "-png", "/work/input", "/work/page"}, dir, 1); err != nil {
		return nil, fmt.Errorf("extract: PDF page rasterization failed: %w", err)
	}
	pngPath := filepath.Join(dir, "page.png")
	defer func() { _ = os.Remove(pngPath) }()
	file, err := os.Open(pngPath) //nolint:gosec // pngPath is pdftoppm's own output inside the private MkdirTemp dir
	if err != nil {
		return nil, fmt.Errorf("%w: PDF page raster missing", ErrMalformedDocument)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxNativeOutput+1))
	_ = file.Close()
	if err != nil {
		return nil, err
	}
	if len(data) > maxNativeOutput {
		return nil, ErrDocumentTooLarge
	}
	pixels, err := imageDimensions(data)
	if err != nil {
		return nil, err
	}
	*totalPixels += pixels
	if pixels > maxImagePixels {
		return nil, fmt.Errorf("%w: PDF page %d is %d megapixels at 150 dpi, the OCR limit is %d", ErrDocumentTooLarge, page, pixels/1_000_000, maxImagePixels/1_000_000)
	}
	if *totalPixels > maxOCRPixels {
		return nil, fmt.Errorf("%w: PDF has too many scanned pages to OCR (limit reached at page %d, about %d letter-size pages)", ErrDocumentTooLarge, page, maxOCRPixels/(1275*1650))
	}
	tesseract, _ := exec.LookPath("tesseract")
	raw, err := runNative(ctx, sandbox, prlimit, tesseract, []string{"/work/page.png", "stdout", "-l", language, "tsv"}, dir, maxNativeOutput)
	if err != nil {
		return nil, err
	}
	blocks, err := parseOCRTSV(raw)
	if err != nil {
		return nil, err
	}
	for i := range blocks {
		blocks[i].locator.Page = new(page)
	}
	return blocks, nil
}

// parsePDFBBox reads pdftotext -bbox output for a page range starting at
// firstPage.
func parsePDFBBox(raw []byte, firstPage int) ([]parseBlock, error) {
	z := htmltoken.NewTokenizer(bytes.NewReader(raw))
	var blocks []parseBlock
	page, textOffset := firstPage-1, 0
	box := ""
	inWord := false
	for {
		kind := z.Next()
		token := z.Token()
		switch kind {
		case htmltoken.ErrorToken:
			if z.Err() == io.EOF && !inWord && page >= firstPage {
				return mergeParagraphLines(mergeLineWords(blocks, false), false), nil
			}
			return nil, errors.New("extract: malformed Poppler bbox output")
		case htmltoken.StartTagToken:
			switch token.Data {
			case "page":
				// Poppler emits number= in some builds and only width/height
				// in others; pages are sequential either way.
				if n, err := strconv.Atoi(tokenAttr(token, "number")); err == nil && n >= 1 {
					page = n
				} else {
					page++
				}
				if page < 1 {
					return nil, errors.New("extract: malformed Poppler page number")
				}
			case "word":
				boxes := []string{tokenAttr(token, "xMin"), tokenAttr(token, "yMin"), tokenAttr(token, "xMax"), tokenAttr(token, "yMax")}
				for _, coordinate := range boxes {
					if _, err := strconv.ParseFloat(coordinate, 64); err != nil {
						return nil, errors.New("extract: malformed Poppler word box")
					}
				}
				if page < firstPage {
					return nil, errors.New("extract: Poppler word without page")
				}
				box = strings.Join(boxes, ",")
				inWord = true
			}
		case htmltoken.TextToken:
			text := strings.TrimSpace(token.Data)
			if inWord && text != "" {
				if textOffset > 0 {
					textOffset++
				}
				blocks = append(blocks, parseBlock{kind: "paragraph", text: text, start: textOffset, end: textOffset + len(text), locator: source.Locator{Page: new(page), Box: box}})
				textOffset += len(text)
			}
		case htmltoken.EndTagToken:
			if token.Data == "word" {
				inWord = false
			}
		}
	}
}

func tokenAttr(token htmltoken.Token, key string) string {
	for _, attr := range token.Attr {
		if strings.EqualFold(attr.Key, key) {
			return attr.Val
		}
	}
	return ""
}

func parseOCRTSV(raw []byte) ([]parseBlock, error) {
	// Trim line ends only: a row without text ends in a tab that is part of
	// its twelve columns, and a blank page's output ends with such a row.
	lines := strings.Split(strings.Trim(string(raw), "\r\n"), "\n")
	if len(lines) < 2 {
		return nil, errors.New("extract: malformed OCR output")
	}
	var blocks []parseBlock
	offset := 0
	for _, line := range lines[1:] {
		cols := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(cols) < 12 {
			return nil, errors.New("extract: malformed OCR output")
		}
		text := strings.TrimSpace(cols[11])
		if text == "" {
			continue
		}
		if cols[0] != "5" || !utf8.ValidString(text) {
			return nil, errors.New("extract: malformed OCR word")
		}
		page, errPage := strconv.Atoi(cols[1])
		left, errLeft := strconv.Atoi(cols[6])
		top, errTop := strconv.Atoi(cols[7])
		width, errWidth := strconv.Atoi(cols[8])
		height, errHeight := strconv.Atoi(cols[9])
		if errPage != nil || errLeft != nil || errTop != nil || errWidth != nil || errHeight != nil || page < 1 || left < 0 || top < 0 || width < 1 || height < 1 {
			return nil, errors.New("extract: malformed OCR coordinates")
		}
		if offset > 0 {
			offset++
		}
		blocks = append(blocks, parseBlock{kind: "paragraph", text: text, start: offset, end: offset + len(text), locator: source.Locator{Page: &page, Box: fmt.Sprintf("%d,%d,%d,%d", left, top, width, height)}})
		offset += len(text)
	}
	return mergeParagraphLines(mergeLineWords(blocks, true), true), nil
}

// mergeLineWords joins consecutive word blocks that sit on one text line into
// a single block: its text is the words separated by spaces and its box their
// union. Poppler and Tesseract report every word separately; kept that way,
// document text reads one word per line and a chunk carries a span per word.
// sized marks Tesseract boxes (left,top,width,height); Poppler's are edges
// (xMin,yMin,xMax,yMax). Offsets are renumbered for the merged blocks.
func mergeLineWords(words []parseBlock, sized bool) []parseBlock {
	type edges struct{ x0, y0, x1, y1 float64 }
	parse := func(box string) (edges, bool) {
		parts := strings.Split(box, ",")
		if len(parts) != 4 {
			return edges{}, false
		}
		var v [4]float64
		for i, part := range parts {
			f, err := strconv.ParseFloat(part, 64)
			if err != nil {
				return edges{}, false
			}
			v[i] = f
		}
		if sized {
			return edges{v[0], v[1], v[0] + v[2], v[1] + v[3]}, true
		}
		return edges{v[0], v[1], v[2], v[3]}, true
	}
	out := make([]parseBlock, 0, len(words)/6+1)
	var line edges
	open := false // out's last block is a line that can take more words
	for _, word := range words {
		box, ok := parse(word.locator.Box)
		if open && ok {
			last := &out[len(out)-1]
			middle, height := (box.y0+box.y1)/2, box.y1-box.y0
			samePage := last.locator.Page != nil && word.locator.Page != nil && *last.locator.Page == *word.locator.Page
			if samePage && middle > line.y0 && middle < line.y1 && box.x0 >= line.x1-height/2 {
				last.text += " " + word.text
				line = edges{min(line.x0, box.x0), min(line.y0, box.y0), max(line.x1, box.x1), max(line.y1, box.y1)}
				if sized {
					last.locator.Box = fmt.Sprintf("%.0f,%.0f,%.0f,%.0f", line.x0, line.y0, line.x1-line.x0, line.y1-line.y0)
				} else {
					last.locator.Box = fmt.Sprintf("%f,%f,%f,%f", line.x0, line.y0, line.x1, line.y1)
				}
				continue
			}
		}
		out = append(out, word)
		line, open = box, ok
	}
	offset := 0
	for i := range out {
		out[i].start, out[i].end = offset, offset+len(out[i].text)
		offset = out[i].end + 1
	}
	return out
}

func imageDimensions(data []byte) (int, error) {
	if len(data) < 24 {
		return 0, ErrMalformedDocument
	}
	w := int(data[16])<<24 | int(data[17])<<16 | int(data[18])<<8 | int(data[19])
	h := int(data[20])<<24 | int(data[21])<<16 | int(data[22])<<8 | int(data[23])
	if w <= 0 || h <= 0 || w > maxImagePixels/h {
		return 0, ErrDocumentTooLarge
	}
	return w * h, nil
}
