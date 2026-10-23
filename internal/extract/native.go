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
	"syscall"
	"unicode/utf8"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/source"
	htmltoken "golang.org/x/net/html"
)

const (
	maxNativeInput  = 32 << 20
	maxNativeOutput = 16 << 20
	maxPDFPages     = 100
	maxImagePixels  = 40_000_000
)

type NativeCapabilities struct {
	PDF       bool
	OCR       bool
	Languages []string
	Reason    string
}

// ProbeNative reports only capabilities available inside the required filesystem and network sandbox.
func ProbeNative() NativeCapabilities {
	nativeProbe.Do(func() {
		sandbox, e1 := exec.LookPath("bwrap")
		prlimit, e2 := exec.LookPath("prlimit")
		if e1 != nil || e2 != nil {
			nativeCaps.Reason = "prlimit and bubblewrap are required for native processing"
			return
		}
		if _, err := runNative(context.Background(), sandbox, prlimit, "/usr/bin/true", nil, "", 1); err != nil {
			nativeCaps.Reason = "native processor sandbox unavailable: " + err.Error()
			return
		}
		_, textErr := exec.LookPath("pdftotext")
		_, rasterErr := exec.LookPath("pdftoppm")
		_, infoErr := exec.LookPath("pdfinfo")
		if textErr == nil && rasterErr == nil && infoErr == nil {
			nativeCaps.PDF = true
		} else {
			nativeCaps.Reason = "Poppler pdfinfo, pdftotext and pdftoppm are required"
		}
		if tesseract, err := exec.LookPath("tesseract"); err == nil {
			nativeCaps.Languages = availableLanguages(tesseract, sandbox, prlimit)
			nativeCaps.OCR = len(nativeCaps.Languages) > 0
		}
		if !nativeCaps.OCR {
			if nativeCaps.Reason != "" {
				nativeCaps.Reason += "; "
			}
			nativeCaps.Reason += "Tesseract or OCR language assets are unavailable"
		}
	})
	caps := nativeCaps
	caps.Languages = append([]string(nil), nativeCaps.Languages...)
	return caps
}

var nativeProbe sync.Once
var nativeCaps NativeCapabilities

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
		if err := os.Chmod(dir, 0700); err != nil {
			cleanup()
			return nil, err
		}
	}
	defer cleanup()
	cmdArgs := []string{"--die-with-parent", "--new-session", "--unshare-all", "--ro-bind", "/usr", "/usr", "--ro-bind", "/lib", "/lib", "--ro-bind", "/lib64", "/lib64", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--bind", dir, "/work", "--chdir", "/work", "--", prlimit, "--cpu=30", "--as=536870912", "--nproc=16", "--fsize=16777216", "--", binary}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, sandbox, cmdArgs...)
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
	if err := ctx.Err(); err != nil {
		return source.Document{}, err
	}
	if len(data) > maxNativeInput {
		return source.Document{}, ErrDocumentTooLarge
	}
	if format == "image" {
		if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
			return source.Document{}, fmt.Errorf("%w: %s is unsupported; only PNG image OCR is supported", ErrUnsupportedFormat, http.DetectContentType(data))
		}
		pixels, err := imageDimensions(data)
		if err != nil {
			return source.Document{}, err
		}
		if pixels > maxImagePixels {
			return source.Document{}, ErrDocumentTooLarge
		}
	}
	caps := ProbeNative()
	if format == "pdf" && !caps.PDF {
		return source.Document{}, fmt.Errorf("extract: PDF unavailable: %s", caps.Reason)
	}
	if format == "image" && !caps.OCR {
		return source.Document{}, fmt.Errorf("extract: OCR unavailable: %s", caps.Reason)
	}
	sandbox, _ := exec.LookPath("bwrap")
	prlimit, _ := exec.LookPath("prlimit")
	dir, err := os.MkdirTemp("", "enclava-native-")
	if err != nil {
		return source.Document{}, err
	}
	defer os.RemoveAll(dir)
	if err = os.Chmod(dir, 0700); err != nil {
		return source.Document{}, err
	}
	if err = os.WriteFile(filepath.Join(dir, "input"), data, 0600); err != nil {
		return source.Document{}, err
	}
	var blocks []parseBlock
	var parseWarnings []string
	if format == "pdf" {
		pdfinfo, _ := exec.LookPath("pdfinfo")
		info, err := runNative(ctx, sandbox, prlimit, pdfinfo, []string{"/work/input"}, dir, 1<<20)
		if err != nil {
			return source.Document{}, fmt.Errorf("%w: PDF metadata: %v", ErrMalformedDocument, err)
		}
		pages, err := pdfPageCount(info)
		if err != nil {
			return source.Document{}, err
		}
		if pages > maxPDFPages {
			return source.Document{}, ErrDocumentTooLarge
		}
		pdftotext, _ := exec.LookPath("pdftotext")
		bbox, err := runNative(ctx, sandbox, prlimit, pdftotext, []string{"-bbox", "-f", "1", "-l", strconv.Itoa(pages), "/work/input", "-"}, dir, maxNativeOutput)
		if err != nil {
			return source.Document{}, fmt.Errorf("%w: PDF text: %v", ErrMalformedDocument, err)
		}
		blocks, err = parsePDFBBox(bbox)
		if err != nil {
			return source.Document{}, err
		}
		hasText := make(map[int]bool)
		for _, block := range blocks {
			if block.locator.Page != nil {
				hasText[*block.locator.Page] = true
			}
		}
		var rasterPixels int
		if !caps.OCR && len(hasText) < pages {
			return source.Document{}, fmt.Errorf("extract: PDF OCR unavailable: %s", caps.Reason)
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
					return source.Document{}, err
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
			return source.Document{}, fmt.Errorf("%w: PDF contains no extractable text", ErrMalformedDocument)
		}
	} else {
		tesseract, _ := exec.LookPath("tesseract")
		raw, err := runNative(ctx, sandbox, prlimit, tesseract, []string{"/work/input", "stdout", "-l", ocrLanguage(caps.Languages), "tsv"}, dir, maxNativeOutput)
		if err != nil {
			return source.Document{}, err
		}
		blocks, err = parseOCRTSV(raw)
		if err != nil {
			return source.Document{}, err
		}
	}
	document := makeDocument(opts.ObjectRef, ref, blocks)
	document.ParseWarnings = parseWarnings
	return document, nil
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
	defer os.Remove(pngPath)
	file, err := os.Open(pngPath)
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
	if pixels > maxImagePixels || *totalPixels > 120_000_000 {
		return nil, ErrDocumentTooLarge
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

func parsePDFBBox(raw []byte) ([]parseBlock, error) {
	z := htmltoken.NewTokenizer(bytes.NewReader(raw))
	var blocks []parseBlock
	page, textOffset := 0, 0
	box := ""
	inWord := false
	for {
		kind := z.Next()
		token := z.Token()
		switch kind {
		case htmltoken.ErrorToken:
			if z.Err() == io.EOF && !inWord && page > 0 {
				return blocks, nil
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
				if page < 1 {
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
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		return nil, errors.New("extract: malformed OCR output")
	}
	var blocks []parseBlock
	offset := 0
	for _, line := range lines[1:] {
		cols := strings.Split(line, "\t")
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
	return blocks, nil
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
