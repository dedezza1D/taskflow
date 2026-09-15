package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/dedezza1D/taskflow/internal/objects"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/dedezza1D/taskflow/internal/worker"
)

// maxOriginalBytes bounds how much of an original we will ever pull into the
// worker — a second line of defense behind the API's upload limit.
const maxOriginalBytes = 64 << 20 // 64 MiB

// maxPDFPages bounds rasterization fan-out. A PDF over the limit is a
// PERMANENT error, never a silent truncation: a compliance report that
// skipped pages would be worse than no report.
const maxPDFPages = 500

// imageExt maps supported image content types to a file extension for the
// tesseract temp file.
var imageExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/tiff": ".tif",
	"image/bmp":  ".bmp",
}

// SupportedContentTypes is the upload allow-list the API enforces, so
// unsupported types fail fast at POST instead of dead-lettering later.
func SupportedContentTypes() []string {
	out := []string{"text/plain", "application/pdf"}
	for ct := range imageExt {
		out = append(out, ct)
	}
	return out
}

func normalizeContentType(ct string) string {
	return strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
}

// runOCR produces the document's text: checkpoint-first, then compute.
//
//   - text/plain passes through (also what keeps CI green without tesseract);
//   - images run tesseract under exec.CommandContext with the stage's own
//     sub-ceiling, so a hung binary is killed cooperatively;
//   - PDFs go through pdfText: the text layer when there is one (no OCR at
//     all), otherwise OCR over the images the file embeds — all under ONE
//     ceiling, producing ONE atomically-written text artifact;
//   - a tool failure that is NOT the ceiling firing is treated as a poison
//     pill (corrupt/malicious document) → permanent → dead-letter, never a
//     crash-loop.
func (p *Pipeline) runOCR(ctx context.Context, doc *store.Document) (string, error) {
	if data, ok, err := p.loadCheckpoint(ctx, doc.ID, StageOCR, KindText); err != nil {
		return "", err
	} else if ok {
		return string(data), nil
	}

	original, err := p.readOriginal(ctx, doc)
	if err != nil {
		return "", err
	}

	ct := normalizeContentType(doc.ContentType)
	var text string
	switch {
	case ct == "text/plain":
		text = string(original)
	case ct == "application/pdf":
		text, err = p.pdfText(ctx, original)
		if err != nil {
			return "", err
		}
	default:
		ext, ok := imageExt[ct]
		if !ok {
			return "", worker.Permanent(fmt.Errorf("unsupported content type %q", ct))
		}
		text, err = p.tesseract(ctx, original, ext)
		if err != nil {
			return "", err
		}
	}

	if err := p.saveCheckpoint(ctx, doc.ID, StageOCR, KindText, objectKey(doc.ID, "ocr.txt"), []byte(text)); err != nil {
		return "", err
	}
	return text, nil
}

func (p *Pipeline) readOriginal(ctx context.Context, doc *store.Document) ([]byte, error) {
	rc, err := p.objects.Get(ctx, doc.StorageURI)
	if errors.Is(err, objects.ErrNotFound) {
		// The original is gone but the document row exists: not retryable — the
		// bytes will not come back.
		return nil, worker.Permanent(fmt.Errorf("original object missing at %s", doc.StorageURI))
	}
	if err != nil {
		return nil, fmt.Errorf("open original: %w", err) // storage hiccup: retryable
	}
	defer rc.Close()

	data, err := io.ReadAll(io.LimitReader(rc, maxOriginalBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read original: %w", err)
	}
	if len(data) > maxOriginalBytes {
		return nil, worker.Permanent(fmt.Errorf("original exceeds %d bytes", maxOriginalBytes))
	}
	return data, nil
}

// tesseract runs OCR on image bytes under the stage sub-ceiling. When called
// per-page from the PDF path, the parent context already carries the stage deadline;
// WithTimeout can only shorten a deadline, never extend it, so the single
// stage ceiling still dominates.
func (p *Pipeline) tesseract(ctx context.Context, image []byte, ext string) (string, error) {
	if p.cfg.OCRTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.cfg.OCRTimeout)
		defer cancel()
	}

	tmp, err := os.CreateTemp("", "taskflow-ocr-*"+ext)
	if err != nil {
		return "", fmt.Errorf("ocr temp file: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	if _, err := tmp.Write(image); err != nil {
		return "", fmt.Errorf("ocr temp write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("ocr temp close: %w", err)
	}

	return p.tesseractFile(ctx, tmp.Name())
}

// tesseractFile runs tesseract on an image already on disk.
func (p *Pipeline) tesseractFile(ctx context.Context, path string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, p.cfg.TesseractBin, path, "stdout", "-l", p.cfg.OCRLanguages)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if runErr != nil {
		// Ceiling (or lease/shutdown) fired: retryable — the next attempt
		// resumes from this stage's missing checkpoint.
		if ctx.Err() != nil {
			return "", fmt.Errorf("ocr cancelled: %w", ctx.Err())
		}
		if errors.Is(runErr, exec.ErrNotFound) {
			// Environment problem, not a document problem: retryable so a fixed
			// deploy picks the work back up.
			return "", fmt.Errorf("tesseract not installed: %w", runErr)
		}
		// Tesseract ran and rejected the input: poison pill → permanent.
		// stderr is bounded and passes through the scrub chokepoint anyway.
		return "", worker.Permanent(fmt.Errorf("tesseract failed (likely corrupt document): %s", firstLine(stderr.String())))
	}
	return stdout.String(), nil
}

// firstLine keeps a tool's stderr short enough to log without dumping a
// document into it.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
