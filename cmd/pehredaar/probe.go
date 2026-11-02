package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/akkuraina/pehredaar/internal/differ"
	"github.com/akkuraina/pehredaar/internal/fetcher"
	"github.com/akkuraina/pehredaar/internal/normalize"
	"github.com/akkuraina/pehredaar/internal/store"
)

// probe is a development tool: it shows exactly what the
// pipeline would see for a URL, so we can judge how noisy a source is
// before trusting it.
type probe struct {
	f       *fetcher.Fetcher
	n       *normalize.Normalizer
	preview int // lines of canonical text to print; 0 = all
}

func runProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	kind := fs.String("kind", "auto", "source kind: auto, feed or pdf")
	again := fs.Duration("again", 0, "fetch a second time after this delay and compare (e.g. 5m)")
	attachments := fs.Int("attachments", 0, "for feeds: also probe the first N attachment PDFs")
	lines := fs.Int("lines", 15, "lines of canonical text to show (0 = all)")
	gap := fs.Duration("gap", 3*time.Second, "minimum delay between requests to one host")
	windowed := fs.Bool("windowed", true, "for feeds: the URL returns one page of a longer list (false for the calendar)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: pehredaar probe [flags] <url>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("probe needs exactly one URL")
	}
	url := fs.Arg(0)

	p := &probe{
		f:       fetcher.New(fetcher.Config{HostInterval: *gap}),
		n:       normalize.New(),
		preview: *lines,
	}

	first, err := p.once(ctx, url, store.Kind(*kind), fetcher.Request{URL: url})
	if err != nil {
		return err
	}

	if *attachments > 0 && first.kind == store.KindFeed {
		p.probeAttachments(ctx, first.doc, *attachments)
	}

	if *again > 0 {
		fmt.Printf("\n... waiting %v before fetching again ...\n\n", *again)
		select {
		case <-time.After(*again):
		case <-ctx.Done():
			return ctx.Err()
		}
		// Conditional GET, exactly as the scheduler will do it.
		second, err := p.once(ctx, url, first.kind, fetcher.Request{
			URL: url, ETag: first.resp.ETag, LastModified: first.resp.LastModified,
		})
		if err != nil {
			return err
		}
		return compare(first, second, differ.Options{Windowed: *windowed})
	}
	return nil
}

type probeResult struct {
	kind store.Kind
	resp *fetcher.Response
	doc  normalize.Doc
}

func (p *probe) once(ctx context.Context, url string, kind store.Kind, req fetcher.Request) (probeResult, error) {
	start := time.Now()
	resp, err := p.f.Fetch(ctx, req)
	if err != nil {
		return probeResult{}, err
	}
	fmt.Printf("GET %s\n", url)
	fmt.Printf("  status        %d (%v)\n", resp.StatusCode, time.Since(start).Round(time.Millisecond))
	fmt.Printf("  etag          %s\n", orDash(resp.ETag))
	fmt.Printf("  last-modified %s\n", orDash(resp.LastModified))
	if resp.NotModified {
		fmt.Println("  304 Not Modified: server says nothing changed, nothing to normalize")
		return probeResult{kind: kind, resp: resp}, nil
	}
	fmt.Printf("  content-type  %s, %d bytes\n", resp.ContentType, len(resp.Body))

	if kind == "auto" {
		kind = detectKind(resp)
	}
	doc, err := p.n.Normalize(ctx, store.Source{URL: url, Kind: kind}, resp.Body, normalize.Rules{})
	if err != nil {
		return probeResult{}, fmt.Errorf("normalize as %s: %w", kind, err)
	}

	n := strings.Count(doc.Text, "\n") + 1
	fmt.Printf("  kind          %s\n", kind)
	fmt.Printf("  canonical     %d lines, %d bytes, hash %s\n", n, len(doc.Text), doc.Hash[:16])
	if kind == store.KindFeed {
		fmt.Printf("  items         %d\n", len(doc.Items))
	}
	printPreview(doc.Text, p.preview)
	return probeResult{kind: kind, resp: resp, doc: doc}, nil
}

func (p *probe) probeAttachments(ctx context.Context, feed normalize.Doc, limit int) {
	done := 0
	for _, it := range feed.Items {
		for _, a := range it.Attachments {
			if done == limit {
				return
			}
			done++
			fmt.Printf("\n=== attachment %d/%d: %s\n", done, limit, a.FileName)
			if _, err := p.once(ctx, a.URL, store.KindPDF, fetcher.Request{URL: a.URL}); err != nil {
				// Keep going: one bad PDF shouldn't hide the others.
				fmt.Printf("  ERROR %v\n", err)
			}
		}
	}
}

// compare reports whether the second fetch would count as a change, using
// the same differ the scheduler uses.
func compare(a, b probeResult, opts differ.Options) error {
	fmt.Println("\n=== comparison")
	switch {
	case b.resp.NotModified:
		fmt.Println("  unchanged (server returned 304)")
	case a.doc.Hash == b.doc.Hash:
		fmt.Println("  unchanged (same canonical hash)")
		if a.resp.ETag != "" && a.resp.ETag != b.resp.ETag {
			fmt.Println("  note: ETag changed although content did not; the ETag is noisy for this source")
		}
	default:
		r, err := differ.Diff(b.kind, a.doc.Text, b.doc.Text, opts)
		if err != nil {
			return err
		}
		if r.Empty() {
			fmt.Printf("  text differs but only as noise (%d blocks ignored): no alert\n", r.Ignored)
			return nil
		}
		fmt.Printf("  CHANGED (%s, %d noise blocks ignored)\n", r.Kind, r.Ignored)
		printPreview(r.Text, 0)
	}
	return nil
}

func detectKind(resp *fetcher.Response) store.Kind {
	switch {
	case strings.Contains(resp.ContentType, "json"):
		return store.KindFeed
	case resp.ContentType == "application/pdf",
		bytes.HasPrefix(resp.Body, []byte("%PDF-")):
		return store.KindPDF
	}
	return store.KindHTML
}

func printPreview(text string, max int) {
	lines := strings.Split(text, "\n")
	shown := lines
	if max > 0 && len(lines) > max {
		shown = lines[:max]
	}
	fmt.Println("  --- canonical text ---")
	for _, l := range shown {
		fmt.Println("  | " + l)
	}
	if len(shown) < len(lines) {
		fmt.Printf("  | ... %d more lines (use -lines 0 to see all)\n", len(lines)-len(shown))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
