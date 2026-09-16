package pipeline

// PDF tests.
//
// Note what does NOT appear here any more: a skip. The old poppler path meant
// every one of these tests was skipped on a machine without pdftoppm/pdfinfo —
// which is to say, on CI. The text-layer path is pure Go, so the cases below
// actually run. Only the scanned-document test still needs tesseract, because
// only that case genuinely involves OCR.

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dedezza1D/taskflow/internal/pii"
	"github.com/dedezza1D/taskflow/internal/worker"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"go.uber.org/zap"
)

// resolveTesseract returns the OCR binary, or fails the test.
//
// It used to skip when tesseract was absent from PATH, which is how the single
// test covering the scanned-document path -- the reason this product needs OCR
// at all -- quietly stopped running on a machine that had tesseract installed
// all along, just not on PATH. So this looks where the Windows installer
// actually puts it before giving up, and gives up loudly.
func resolveTesseract(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("tesseract"); err == nil {
		return p
	}
	for _, dir := range []string{
		filepath.Join(os.Getenv("ProgramFiles"), "Tesseract-OCR"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Tesseract-OCR"),
		"/usr/bin", "/usr/local/bin", "/opt/homebrew/bin",
	} {
		if dir == "" {
			continue
		}
		for _, name := range []string{"tesseract", "tesseract.exe"} {
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	t.Fatalf("tesseract is required by this test and was not found on PATH " +
		"or in the usual install locations; install it (winget install " +
		"UB-Mannheim.TesseractOCR, or your package manager)")
	return ""
}

func newPDFTestPipeline() *Pipeline {
	// The PDF path touches neither the store nor object storage, so nils are fine.
	return New(nil, nil, zap.NewNop(), Config{OCRTimeout: 2 * time.Minute})
}

// buildPDF constructs a minimal valid PDF, one Helvetica text line per page.
// Object layout: 1=catalog, 2=pages, then (page,content) pairs, font last.
func buildPDF(pageTexts []string) []byte {
	var buf bytes.Buffer
	var offsets []int
	addObj := func(body string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}

	buf.WriteString("%PDF-1.4\n")

	n := len(pageTexts)
	kids := make([]string, n)
	for i := range pageTexts {
		kids[i] = fmt.Sprintf("%d 0 R", 3+2*i)
	}
	fontNum := 3 + 2*n

	addObj("<< /Type /Catalog /Pages 2 0 R >>")
	addObj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	for i, text := range pageTexts {
		contentNum := 3 + 2*i + 1
		addObj(fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>",
			fontNum, contentNum))
		stream := fmt.Sprintf("BT /F1 32 Tf 72 700 Td (%s) Tj ET", text)
		addObj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	addObj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xrefPos)
	return buf.Bytes()
}

// buildScannedPDF produces a PDF whose only content is an embedded image — what
// a scanner emits, and the one case that genuinely needs OCR.
func buildScannedPDF(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()

	img := image.NewRGBA(image.Rect(0, 0, 600, 200))
	for x := 0; x < 600; x++ {
		for y := 0; y < 200; y++ {
			img.Set(x, y, color.White)
		}
	}
	// A crude glyph-ish mark; the OCR assertion below only checks that the
	// scanned path RAN, not what it read.
	for x := 50; x < 550; x++ {
		for y := 95; y < 105; y++ {
			img.Set(x, y, color.Black)
		}
	}

	imgPath := filepath.Join(dir, "scan.png")
	f, err := os.Create(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()

	pdfPath := filepath.Join(dir, "scanned.pdf")
	if err := api.ImportImagesFile([]string{imgPath}, pdfPath, nil, nil); err != nil {
		t.Fatalf("ImportImagesFile: %v", err)
	}
	data, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A digital PDF is read straight off its text layer: exact, fast, and with no
// OCR to misread characters that were never ambiguous.
func TestPDFTextLayerNeedsNoOCR(t *testing.T) {
	p := newPDFTestPipeline()

	text, err := p.pdfText(context.Background(), buildPDF([]string{"CPF 529.982.247-25"}))
	if err != nil {
		t.Fatalf("pdfText: %v", err)
	}
	if !strings.Contains(text, "529.982.247-25") {
		t.Fatalf("text layer did not come through; got %q", text)
	}
}

func TestPDFMultiPageJoinsInOrder(t *testing.T) {
	p := newPDFTestPipeline()

	text, err := p.pdfText(context.Background(), buildPDF([]string{"ALPHA FIRST", "BRAVO SECOND"}))
	if err != nil {
		t.Fatalf("pdfText: %v", err)
	}
	first := strings.Index(text, "ALPHA")
	second := strings.Index(text, "BRAVO")
	if first < 0 || second < 0 {
		t.Fatalf("missing page content; got %q", text)
	}
	if first > second {
		t.Fatalf("pages out of order; got %q", text)
	}
	if !strings.Contains(text, "\f") {
		t.Fatalf("expected form-feed page separator; got %q", text)
	}
}

// A file that is not a PDF cannot become one by retrying.
func TestPDFCorruptIsPermanent(t *testing.T) {
	p := newPDFTestPipeline()

	_, err := p.pdfText(context.Background(), []byte("this is not a pdf at all"))
	if err == nil {
		t.Fatal("expected an error for a corrupt pdf")
	}
	if !worker.IsPermanent(err) {
		t.Fatalf("a corrupt pdf must be a permanent (poison pill) error, got: %v", err)
	}
}

// The page ceiling is checked before any extraction work, so an oversized
// document is rejected cheaply rather than after burning the stage budget.
func TestPDFOverPageLimitIsPermanent(t *testing.T) {
	p := newPDFTestPipeline()

	pages := make([]string, maxPDFPages+1)
	for i := range pages {
		pages[i] = fmt.Sprintf("PAGE %d", i)
	}

	_, err := p.pdfText(context.Background(), buildPDF(pages))
	if err == nil {
		t.Fatal("expected an error for a pdf over the page limit")
	}
	if !worker.IsPermanent(err) {
		t.Fatalf("over-limit must be permanent, got: %v", err)
	}
	if !strings.Contains(err.Error(), "page limit") {
		t.Errorf("error should name the limit, got: %v", err)
	}
}

// A PDF with neither text nor pictures has nothing this pipeline can read.
func TestPDFWithNoContentIsPermanent(t *testing.T) {
	p := newPDFTestPipeline()

	// A page with an empty content stream: valid PDF, nothing in it.
	_, err := p.pdfText(context.Background(), buildPDF([]string{""}))
	if err == nil {
		t.Fatal("expected an error for a pdf with no readable content")
	}
	if !worker.IsPermanent(err) {
		t.Fatalf("must be permanent, got: %v", err)
	}
}

// The scanned path: no text layer, so the embedded images are pulled out and
// OCR'd. This is the only PDF test that needs an external binary.
func TestPDFScannedFallsBackToOCR(t *testing.T) {
	p := New(nil, nil, zap.NewNop(), Config{
		OCRTimeout:   2 * time.Minute,
		TesseractBin: resolveTesseract(t),
	})

	// It must not be mistaken for a text-layer document.
	data := buildScannedPDF(t)
	if _, ok := pdfTextLayer(writeTemp(t, data)); ok {
		t.Fatal("a scanned pdf must not be treated as having a text layer")
	}

	if _, err := p.pdfText(context.Background(), data); err != nil {
		t.Fatalf("scanned pdf should go through OCR: %v", err)
	}
}

// Even without tesseract, the routing decision itself is testable: a scan has
// no text layer, a digital document does.
func TestPDFTextLayerDetection(t *testing.T) {
	digital := writeTemp(t, buildPDF([]string{"A LINE OF REAL TEXT IN THIS DOCUMENT"}))
	if _, ok := pdfTextLayer(digital); !ok {
		t.Error("a digital pdf should be detected as having a text layer")
	}

	scanned := writeTemp(t, buildScannedPDF(t))
	if _, ok := pdfTextLayer(scanned); ok {
		t.Error("a scanned pdf should not be detected as having a text layer")
	}
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSupportedContentTypesIncludesPDF(t *testing.T) {
	for _, ct := range SupportedContentTypes() {
		if ct == "application/pdf" {
			return
		}
	}
	t.Fatal("application/pdf missing from SupportedContentTypes")
}

// buildMultiLinePDF puts several lines on ONE page, which is what a real
// document looks like and what buildPDF (one line per page) never produced.
func buildMultiLinePDF(lines []string) []byte {
	var buf bytes.Buffer
	var offsets []int
	addObj := func(body string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}

	buf.WriteString("%PDF-1.4\n")

	var content strings.Builder
	content.WriteString("BT\n")
	y := 700
	for _, line := range lines {
		fmt.Fprintf(&content, "/F1 12 Tf 1 0 0 1 72 %d Tm (%s) Tj\n", y, line)
		y -= 24
	}
	content.WriteString("ET\n")

	addObj("<< /Type /Catalog /Pages 2 0 R >>")
	addObj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	addObj("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>")
	addObj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", content.Len(), content.String()))
	addObj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xrefPos)
	return buf.Bytes()
}

// The text layer must come back with its LINES intact. Extracted as one run,
// the last value on a line is glued to the first word of the next
// ("...529.982.247-25E-mail:"), which destroys the word boundary every detector
// ends on: the document then reports clean while carrying a CPF, a CNPJ, a card
// and an IBAN. This is the regression test for that.
func TestPDFTextLayerKeepsLinesApart(t *testing.T) {
	p := newPDFTestPipeline()

	lines := []string{
		"CPF: 529.982.247-25",
		"E-mail: maria@exemplo.com.br",
		"CNPJ: 11.222.333/0001-81",
		"Cartao: 4111 1111 1111 1111",
		"IBAN: DE89 3704 0044 0532 0130 00",
		"Fim do documento.",
	}

	text, err := p.pdfText(context.Background(), buildMultiLinePDF(lines))
	if err != nil {
		t.Fatalf("pdfText: %v", err)
	}
	for _, line := range lines {
		if !strings.Contains(text, line) {
			t.Fatalf("line %q did not survive extraction; got %q", line, text)
		}
	}
	if strings.Contains(text, "247-25E-mail") {
		t.Fatalf("lines were glued together: %q", text)
	}

	found := map[string]int{}
	for _, f := range pii.Detect(text) {
		found[f.Category]++
	}
	for _, want := range []string{pii.CategoryCPF, pii.CategoryCNPJ, pii.CategoryCreditCard, pii.CategoryIBAN, pii.CategoryEmail} {
		if found[want] == 0 {
			t.Errorf("%s went undetected in a PDF that contains one (found %v)", want, found)
		}
	}
}

// helveticaWithWidths is the font the positioned fixture uses. Real documents
// carry width metrics; without them the reader reports a run's width as zero
// and the extractor cannot tell a column gap from the next glyph, which is
// exactly the case writeRow refuses to guess about. 556/1000 em for every
// character is close enough to Helvetica to make the arithmetic predictable.
var helveticaWithWidths = func() string {
	widths := make([]string, 0, 95)
	for c := 32; c <= 126; c++ {
		widths = append(widths, "556")
	}
	return "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /FirstChar 32 /LastChar 126 /Widths [" +
		strings.Join(widths, " ") + "] >>"
}()

// pdfRun is one text run on a page: where it starts, and what it says.
type pdfRun struct {
	X, Y int
	Text string
}

// buildPositionedPDF places runs at explicit coordinates, which is the only way
// to reproduce a two-column layout — the case where two values share a line.
func buildPositionedPDF(runs []pdfRun) []byte {
	var buf bytes.Buffer
	var offsets []int
	addObj := func(body string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}

	buf.WriteString("%PDF-1.4\n")

	var content strings.Builder
	content.WriteString("BT\n")
	for _, r := range runs {
		fmt.Fprintf(&content, "/F1 12 Tf 1 0 0 1 %d %d Tm (%s) Tj\n", r.X, r.Y, r.Text)
	}
	content.WriteString("ET\n")

	addObj("<< /Type /Catalog /Pages 2 0 R >>")
	addObj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	addObj("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>")
	addObj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", content.Len(), content.String()))
	addObj(helveticaWithWidths)

	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xrefPos)
	return buf.Bytes()
}

// Two columns on the same line arrive as two runs with a gap between them and
// no space glyph, so the end of the left column used to be glued to the start
// of the right one — the same lost word boundary that hid values at the end of
// a line, one axis over.
func TestPDFTextLayerSeparatesColumns(t *testing.T) {
	p := newPDFTestPipeline()

	text, err := p.pdfText(context.Background(), buildPositionedPDF([]pdfRun{
		{X: 72, Y: 700, Text: "CPF: 529.982.247-25"},
		{X: 320, Y: 700, Text: "Cartao: 4111 1111 1111 1111"},
		{X: 72, Y: 676, Text: "Documento de teste com duas colunas."},
	}))
	if err != nil {
		t.Fatalf("pdfText: %v", err)
	}

	if strings.Contains(text, "25Cartao") {
		t.Fatalf("columns were glued together: %q", text)
	}

	found := map[string]int{}
	for _, f := range pii.Detect(text) {
		found[f.Category]++
	}
	if found[pii.CategoryCPF] == 0 {
		t.Errorf("the CPF ending the left column went undetected (found %v, text %q)", found, text)
	}
	if found[pii.CategoryCreditCard] == 0 {
		t.Errorf("the card in the right column went undetected (found %v, text %q)", found, text)
	}
}

// The gap rule must not fire on ordinary kerning: a value drawn as several
// runs is still one value, and a space inside it would break the detector the
// rule exists to feed.
func TestPDFTextLayerKeepsAValueWhole(t *testing.T) {
	p := newPDFTestPipeline()

	// Adjacent runs: 12 characters at 556/1000 em of 12pt is 80pt, so the second
	// run starts where the first ends - the next glyph, not a word break.
	text, err := p.pdfText(context.Background(), buildPositionedPDF([]pdfRun{
		{X: 72, Y: 700, Text: "CPF: 529.982"},
		{X: 152, Y: 700, Text: ".247-25"},
		{X: 72, Y: 676, Text: "Documento de teste com um valor partido."},
	}))
	if err != nil {
		t.Fatalf("pdfText: %v", err)
	}
	if !strings.Contains(text, "529.982.247-25") {
		t.Fatalf("a value split across runs came back broken: %q", text)
	}
	found := 0
	for _, f := range pii.Detect(text) {
		if f.Category == pii.CategoryCPF {
			found++
		}
	}
	if found == 0 {
		t.Errorf("CPF split across two runs went undetected: %q", text)
	}
}
