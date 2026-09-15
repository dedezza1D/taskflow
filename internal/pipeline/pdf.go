package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dedezza1D/taskflow/internal/worker"
	"github.com/ledongthuc/pdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
)

// PDF handling, in pure Go.
//
// This replaced poppler (pdftoppm + pdfinfo), and the reason was originally a
// licence one: poppler is GPL-2, and shipping it inside a desktop installer
// creates obligations that are a legal decision rather than an engineering one.
// pdfcpu is Apache-2.0 and ledongthuc/pdf is BSD-3, so the question disappears.
//
// But the replacement is also simply better work, and that is the part worth
// keeping in mind. The old path rasterised EVERY page at 300dpi and ran OCR over
// the result — even when the PDF already carried a perfectly good text layer.
// For a digital PDF that is slower, lossier (OCR misreads characters that were
// never ambiguous) and entirely avoidable.
//
// So the strategy is layered:
//
//  1. Read the text layer. Most PDFs in an office have one, and if it yields
//     real text the document is done — no OCR, no image work, milliseconds.
//  2. Otherwise the pages are pictures (a scan), so pull the EMBEDDED images out
//     and OCR those. Extracting what is already in the file beats re-rendering
//     the page to guess at it.
//
// Being pure Go also keeps the desktop build cross-compilable without a C
// toolchain, which a cgo-based rasteriser would have cost us.

// minTextLayerChars is how much extracted text counts as "this PDF has a real
// text layer". Scanners sometimes leave a few stray characters — a page number,
// a watermark — so a nonzero result is not by itself proof. Set low enough that
// a sparse but genuine page still qualifies.
const minTextLayerChars = 24

// pdfText turns a PDF into text, preferring its text layer and falling back to
// OCR over the images it contains.
func (p *Pipeline) pdfText(ctx context.Context, data []byte) (string, error) {
	if p.cfg.OCRTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.cfg.OCRTimeout)
		defer cancel()
	}

	dir, err := os.MkdirTemp("", "taskflow-pdf-*")
	if err != nil {
		return "", fmt.Errorf("pdf temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	pdfPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(pdfPath, data, 0o600); err != nil {
		return "", fmt.Errorf("pdf temp write: %w", err)
	}

	// Page count first, so an over-limit document is rejected before any
	// expensive work — the same ordering the poppler path had.
	pages, err := pdfPageCount(pdfPath)
	if err != nil {
		return "", err
	}
	if pages == 0 {
		return "", worker.Permanent(errors.New("pdf has no pages"))
	}
	if pages > maxPDFPages {
		return "", worker.Permanent(fmt.Errorf("pdf has %d pages, over the %d-page limit", pages, maxPDFPages))
	}

	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("pdf processing cancelled: %w", err)
	}

	// 1. The cheap, exact path.
	text, trusted := pdfTextLayer(pdfPath)
	if trusted {
		return text, nil
	}

	// 2. Not enough text to trust, so treat it as a scan and OCR the images the
	//    file already carries.
	ocrText, err := p.ocrEmbeddedImages(ctx, pdfPath)
	if err == nil {
		return ocrText, nil
	}

	// 3. No images either. If the text layer gave us *something* — a sparse page
	//    is still a page — that beats dead-lettering a document we can partly
	//    read. Only a file with neither is genuinely unreadable.
	if strings.TrimSpace(text) != "" {
		return text, nil
	}
	return "", err
}

// pdfPageCount replaces pdfinfo. A parse failure here means the file is not a
// usable PDF, which is a poison pill rather than something to retry.
func pdfPageCount(path string) (int, error) {
	n, err := api.PageCountFile(path)
	if err != nil {
		return 0, worker.Permanent(fmt.Errorf("unreadable pdf: %w", err))
	}
	return n, nil
}

// pdfTextLayer returns the document's embedded text and whether there was
// enough of it to trust as the document's actual content.
//
// The distinction matters in one direction: a scan that carries a stray
// watermark or page number would, if trusted, skip OCR and silently hide
// everything the page really says — including the personal data this pipeline
// exists to find. So the bar is "enough text to be the content", and anything
// under it is treated as a scan. The caller still keeps the short text as a
// fallback, so being strict here costs nothing.
//
// Errors are deliberately swallowed into ok=false: a PDF that pdfcpu counts
// pages for but this reader cannot parse is not corrupt, it is just one the OCR
// path should handle. Failing here would dead-letter documents that are
// perfectly processable.
func pdfTextLayer(path string) (string, bool) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	var sb strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		page := r.Page(i)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			return "", false
		}
		if i > 1 {
			sb.WriteByte('\f') // page separator, same convention as pdftotext
		}
		sb.WriteString(text)
	}

	out := sb.String()
	if len(strings.TrimSpace(strings.ReplaceAll(out, "\f", ""))) < minTextLayerChars {
		// Hand the text back anyway. It is not trusted as the document's
		// content, but the caller keeps it as a last resort for the case where
		// there turns out to be nothing to OCR either.
		return out, false
	}
	return out, true
}

// ocrEmbeddedImages pulls the pictures out of a scanned PDF and OCRs them.
//
// Extracting beats rasterising: the bytes are already there at their original
// resolution, so there is no render step to get wrong and no dpi to guess.
func (p *Pipeline) ocrEmbeddedImages(ctx context.Context, pdfPath string) (string, error) {
	f, err := os.Open(pdfPath)
	if err != nil {
		return "", fmt.Errorf("reopen pdf: %w", err)
	}
	defer f.Close()

	perPage, err := api.ExtractImagesRaw(f, nil, nil)
	if err != nil {
		return "", worker.Permanent(fmt.Errorf("pdf image extraction failed (likely corrupt pdf): %w", err))
	}

	type pageImage struct {
		page int
		name string
		data []byte
	}
	var images []pageImage

	for i, page := range perPage {
		for _, img := range page {
			buf := new(bytes.Buffer)
			if _, err := buf.ReadFrom(img); err != nil {
				return "", fmt.Errorf("read embedded image: %w", err)
			}
			images = append(images, pageImage{page: i, name: img.Name, data: buf.Bytes()})
		}
	}

	if len(images) == 0 {
		// No text layer and no pictures: there is nothing this pipeline can
		// read. Permanent, because retrying changes nothing.
		return "", worker.Permanent(errors.New("pdf has neither a text layer nor embedded images"))
	}

	// Page order, then a stable order within a page.
	sort.SliceStable(images, func(a, b int) bool {
		if images[a].page != images[b].page {
			return images[a].page < images[b].page
		}
		return images[a].name < images[b].name
	})

	dir := filepath.Dir(pdfPath)
	var sb strings.Builder
	lastPage := -1

	for i, im := range images {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("pdf ocr cancelled: %w", err)
		}

		ext := imageExtForName(im.name)
		imgPath := filepath.Join(dir, fmt.Sprintf("img-%04d%s", i, ext))
		if err := os.WriteFile(imgPath, im.data, 0o600); err != nil {
			return "", fmt.Errorf("write embedded image: %w", err)
		}

		text, err := p.tesseractFile(ctx, imgPath)
		if err != nil {
			return "", fmt.Errorf("pdf page %d: %w", im.page, err)
		}

		if lastPage != -1 && im.page != lastPage {
			sb.WriteByte('\f')
		} else if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(text)
		lastPage = im.page
	}

	return sb.String(), nil
}

// imageExtForName picks the extension tesseract should see. pdfcpu names
// extracted images with their real format; anything unrecognised goes to PNG,
// which tesseract sniffs anyway.
func imageExtForName(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return ".jpg"
	case ".tif", ".tiff":
		return ".tif"
	case ".png":
		return ".png"
	default:
		return ".png"
	}
}
