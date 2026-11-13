package extract

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/source"
)

func TestNativeRealDocumentFixtures(t *testing.T) {
	caps := ProbeNative()
	pdf, err := os.ReadFile("testdata/legal-document.pdf")
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile("testdata/scanned-page.png")
	if err != nil {
		t.Fatal(err)
	}
	pdfParser, pdfRef := testParser(t, pdf, "application/pdf")
	pdfDoc, pdfErr := pdfParser.Parse(context.Background(), pdfRef, ParseOptions{Filename: "legal-document.pdf", MIMEType: "application/pdf", ObjectRef: source.ObjectRef{TenantID: "tenant", ObjectID: "pdf", ObservedSeq: 1}})
	if !caps.PDF {
		if pdfErr == nil || !strings.Contains(pdfErr.Error(), "unavailable") {
			t.Fatalf("missing native isolation must fail closed, got %v", pdfErr)
		}
	} else {
		if pdfErr != nil {
			t.Fatalf("parse real PDF fixture: %v", pdfErr)
		}
		if len(pdfDoc.ParseWarnings) == 0 || !strings.Contains(pdfDoc.ParseWarnings[0], "page 1 contains embedded text; scanned regions not OCR'd") {
			t.Fatalf("PDF preview omitted its partial OCR warning: %#v", pdfDoc.ParseWarnings)
		}
		assertNativeText(t, pdfDoc, "LEGAL EVIDENCE FIXTURE")
		if pdfDoc.Blocks[0].Locator.Page == nil || *pdfDoc.Blocks[0].Locator.Page != 1 || pdfDoc.Blocks[0].Locator.Box == "" {
			t.Fatalf("PDF page/box location missing: %#v", pdfDoc.Blocks[0].Locator)
		}
	}
	imageParser, imageRef := testParser(t, image, "image/png")
	imageDoc, imageErr := imageParser.Parse(context.Background(), imageRef, ParseOptions{Filename: "scanned-page.png", MIMEType: "image/png", ObjectRef: source.ObjectRef{TenantID: "tenant", ObjectID: "image", ObservedSeq: 1}})
	if !caps.OCR {
		if imageErr == nil || !strings.Contains(imageErr.Error(), "unavailable") {
			t.Fatalf("missing OCR isolation/assets must fail closed, got %v", imageErr)
		}
	} else {
		if imageErr != nil {
			t.Fatalf("OCR scanned fixture: %v", imageErr)
		}
		assertNativeText(t, imageDoc, "LEGAL EVIDENCE FIXTURE")
		if imageDoc.Blocks[0].Locator.Page == nil || *imageDoc.Blocks[0].Locator.Page != 1 || imageDoc.Blocks[0].Locator.Box == "" {
			t.Fatalf("OCR page/box location missing: %#v", imageDoc.Blocks[0].Locator)
		}
	}
}

func TestNativeRejectsUnsupportedImageByContent(t *testing.T) {
	_, err := parseNative(context.Background(), artifactsRef(), ParseOptions{}, []byte{0xff, 0xd8, 0xff, 0xd9}, "image")
	if !errors.Is(err, ErrUnsupportedFormat) || !strings.Contains(err.Error(), "image/jpeg") {
		t.Fatalf("JPEG input should be rejected with its actual format, got %v", err)
	}
}

func assertNativeText(t *testing.T, doc source.Document, want string) {
	t.Helper()
	var text strings.Builder
	for _, block := range doc.Blocks {
		text.WriteString(block.Text)
		text.WriteByte(' ')
	}
	if !strings.Contains(strings.ToUpper(text.String()), want) {
		t.Fatalf("expected %q in extracted text %q", want, text.String())
	}
}

func TestNativeMalformedOversizedAndCancelledInputs(t *testing.T) {
	oversized := make([]byte, 24)
	copy(oversized, []byte("\x89PNG\r\n\x1a\n"))
	for i := 16; i < 24; i++ {
		oversized[i] = 0xff
	}
	if _, err := imageDimensions(oversized); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("oversized image error=%v", err)
	}
	if _, err := parsePDFBBox([]byte("<html><word>broken")); err == nil {
		t.Fatal("malformed bbox output was accepted")
	}
	if _, err := parseOCRTSV([]byte("not TSV")); err == nil {
		t.Fatal("malformed OCR output was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := parseNative(ctx, artifactsRef(), ParseOptions{}, nil, "pdf")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled parser error=%v", err)
	}
}

func TestOCRTSVPreservesWordPageAndBox(t *testing.T) {
	raw := []byte("level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n5\t1\t1\t1\t1\t1\t25\t40\t100\t20\t95.5\tEvidence")
	blocks, err := parseOCRTSV(raw)
	if err != nil || len(blocks) != 1 {
		t.Fatalf("OCR TSV blocks=%#v err=%v", blocks, err)
	}
	if blocks[0].text != "Evidence" || blocks[0].locator.Page == nil || *blocks[0].locator.Page != 1 || blocks[0].locator.Box != "25,40,100,20" {
		t.Fatalf("OCR provenance missing: %#v", blocks[0])
	}
}

func TestNativeSandboxDeniesExternalSentinelAndNetwork(t *testing.T) {
	caps := ProbeNative()
	if !caps.PDF { // No isolation means no dispatch, including the fixed binaries.
		if _, err := runNative(context.Background(), "/missing/bwrap", "/missing/prlimit", "/usr/bin/cat", nil, "", 1); err == nil {
			t.Fatal("native child dispatched without verified isolation")
		}
		return
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("external-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runNative(context.Background(), mustLookPath(t, "bwrap"), mustLookPath(t, "prlimit"), "/usr/bin/cat", []string{sentinel}, "", 1024); err == nil {
		t.Fatal("sandbox read an external sentinel")
	}
	httpBinary, err := exec.LookPath("curl")
	httpArgs := make([]string, 0, 5)
	httpArgs = append(httpArgs, "--connect-timeout", "0.2", "--max-time", "0.5")
	if err != nil {
		httpBinary, err = exec.LookPath("wget")
		httpArgs = append(httpArgs[:0], "-T", "1", "-O", "-")
	}
	if err != nil {
		t.Fatal("curl or wget is required for the isolated-network integration check")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second))
	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
			accepted <- struct{}{}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	httpArgs = append(httpArgs, "http://"+listener.Addr().String())
	_, err = runNative(ctx, mustLookPath(t, "bwrap"), mustLookPath(t, "prlimit"), httpBinary, httpArgs, "", 1024)
	select {
	case <-accepted:
		t.Fatal("sandbox reached host HTTP listener")
	default:
	}
	if err == nil {
		t.Fatal("network probe unexpectedly succeeded")
	}
}

func TestNativeCancellationAndFailClosedDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if !ProbeNative().PDF {
		if _, err := runNative(ctx, "/missing/bwrap", "/missing/prlimit", "/usr/bin/sleep", []string{"30"}, "", 1024); err == nil {
			t.Fatal("native child dispatched without verified isolation")
		}
		return
	}
	_, err := runNative(ctx, mustLookPath(t, "bwrap"), mustLookPath(t, "prlimit"), mustLookPath(t, "sleep"), []string{"30"}, "", 1024)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error=%v", err)
	}
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("%s unavailable: %v", name, err)
	}
	return path
}

func artifactsRef() artifacts.Ref {
	return artifacts.Ref{TenantID: "tenant", ID: "artifact", Kind: artifacts.KindOriginal}
}

// A blank page yields only structural rows whose text column is empty; that
// is an empty page, not malformed output that fails the whole document.
func TestParseOCRTSVBlankPage(t *testing.T) {
	raw := []byte("level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
		"1\t1\t0\t0\t0\t0\t0\t0\t828\t1198\t-1\t\n")
	blocks, err := parseOCRTSV(raw)
	if err != nil || len(blocks) != 0 {
		t.Fatalf("blank page: blocks=%v err=%v", blocks, err)
	}
}

// Words on one line become one block; a new line, column or page starts
// another. Single words keep their original box.
func TestPDFWordsMergeIntoLines(t *testing.T) {
	raw := []byte(`<html><body><doc><page width="612" height="792">
<word xMin="72.000000" yMin="100.000000" xMax="100.000000" yMax="112.000000">Maneuver</word>
<word xMin="104.000000" yMin="100.000000" xMax="140.000000" yMax="112.000000">warfare</word>
<word xMin="72.000000" yMin="116.000000" xMax="90.000000" yMax="128.000000">is</word>
</page><page width="612" height="792">
<word xMin="72.000000" yMin="116.000000" xMax="95.000000" yMax="128.000000">next</word>
</page></doc></body></html>`)
	blocks, err := parsePDFBBox(raw)
	if err != nil || len(blocks) != 3 {
		t.Fatalf("blocks=%#v err=%v", blocks, err)
	}
	if blocks[0].text != "Maneuver warfare" || blocks[0].locator.Box != "72.000000,100.000000,140.000000,112.000000" || *blocks[0].locator.Page != 1 {
		t.Fatalf("merged line: %#v", blocks[0])
	}
	if blocks[1].text != "is" || blocks[1].locator.Box != "72.000000,116.000000,90.000000,128.000000" || blocks[1].start != 17 || blocks[1].end != 19 {
		t.Fatalf("second line: %#v", blocks[1])
	}
	if blocks[2].text != "next" || *blocks[2].locator.Page != 2 {
		t.Fatalf("next page: %#v", blocks[2])
	}

	ocr := []byte("level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
		"5\t1\t1\t1\t1\t1\t25\t40\t100\t20\t95\tScanned\n5\t1\t1\t1\t1\t2\t130\t41\t60\t20\t95\tline\n5\t1\t1\t1\t2\t1\t25\t70\t50\t20\t95\tbelow")
	blocks, err = parseOCRTSV(ocr)
	if err != nil || len(blocks) != 2 || blocks[0].text != "Scanned line" || blocks[0].locator.Box != "25,40,165,21" || blocks[1].text != "below" {
		t.Fatalf("OCR lines: %#v err=%v", blocks, err)
	}
}
