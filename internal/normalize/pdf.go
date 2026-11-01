package normalize

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// normalizePDF extracts text with pdftotext and canonicalizes it.
//
// We shell out instead of using a Go PDF library because pdftotext (xpdf or
// poppler) handles far more real-world PDFs, and -layout keeps table rows
// together, which matters for date tables in corrigenda.
func (n *Normalizer) normalizePDF(ctx context.Context, body []byte, rules Rules) (Doc, error) {
	// Servers sometimes return an HTML error page with status 200. The PDF
	// spec allows junk before the header, so look in the first KiB.
	if !bytes.Contains(body[:min(len(body), 1024)], []byte("%PDF-")) {
		return Doc{}, fmt.Errorf("%w: body is not a PDF", ErrNotContent)
	}

	raw, err := n.pdfToText(ctx, body)
	if err != nil {
		return Doc{}, err
	}
	text := CleanText(raw, slices.Concat(defaultPDFDrops, rules.DropLines))
	if text == "" {
		return Doc{}, ErrNoText
	}
	return Doc{Text: text}, nil
}

func (n *Normalizer) pdfToText(ctx context.Context, body []byte) (string, error) {
	// pdftotext needs a real file (PDF parsing seeks backwards from the end,
	// so it can't read a pipe).
	f, err := os.CreateTemp("", "pehredaar-*.pdf")
	if err != nil {
		return "", fmt.Errorf("normalize: temp file: %w", err)
	}
	defer os.Remove(f.Name())
	_, werr := f.Write(body)
	// Close before running pdftotext: on Windows an open handle can block
	// another process from reading the file.
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", fmt.Errorf("normalize: write temp file: %w", werr)
	}

	if n.PDFTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, n.PDFTimeout)
		defer cancel()
	}
	// "-" as the output file means stdout, for both xpdf and poppler.
	// exec.CommandContext kills the process if ctx is cancelled.
	cmd := exec.CommandContext(ctx, n.PDFToText, "-layout", "-enc", "UTF-8", f.Name(), "-")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("normalize: pdftotext: %w", ctx.Err())
		}
		return "", fmt.Errorf("normalize: pdftotext: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
