package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/store"
	"github.com/akkuraina/pehredaar/internal/store/sqlite"
	"github.com/akkuraina/pehredaar/internal/summarizer"
)

// runSummarize is a development tool for tuning the LLM prompt on real
// changes: it prints the alert a change would produce, without saving the
// summary or sending anything. With no change ID it lists recent changes.
func runSummarize(ctx context.Context, args []string) error {
	if _, err := config.LoadDotEnv(".env"); err != nil {
		return err
	}
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	dbPath := fs.String("db", envOr("PEHREDAAR_DB", filepath.Join("data", "pehredaar.db")), "SQLite database file")
	lang := fs.String("lang", "en", "alert language: en or hi")
	limit := fs.Int("n", 20, "changes to list when no ID is given")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: pehredaar summarize [flags] [change-id]")
		fmt.Fprintln(fs.Output(), "Prints the alert for a change without saving or sending it; without an ID, lists recent changes.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lang != string(store.LangEnglish) && *lang != string(store.LangHindi) {
		return fmt.Errorf("-lang must be en or hi, not %q", *lang)
	}

	// Read-only use, but Open also creates the file: refuse a typo'd path.
	if _, err := os.Stat(*dbPath); err != nil {
		return fmt.Errorf("database %s: %w (run `pehredaar run` first)", *dbPath, err)
	}
	st, err := sqlite.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if fs.NArg() == 0 {
		return listChanges(ctx, st, *limit)
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		return fmt.Errorf("change ID %q is not a number", fs.Arg(0))
	}
	ch, err := st.Change(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no change with ID %d; run `pehredaar summarize` to list them", id)
	}
	if err != nil {
		return err
	}
	src, err := st.Source(ctx, ch.SourceID)
	if err != nil {
		return err
	}

	var sum summarizer.Summarizer = summarizer.Template{}
	model := "template (ANTHROPIC_API_KEY not set)"
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		model = envOr("PEHREDAAR_LLM_MODEL", summarizer.DefaultModel)
		sum = summarizer.NewClaude(key, model, nil)
	}
	s, err := sum.Summarize(ctx, summarizer.Input{Change: ch, Source: src, Lang: store.Language(*lang)})
	if err != nil {
		return err
	}
	fmt.Printf("change %d · %s · %s\nsummarizer: %s\nimportance: %s\n\n%s\n",
		ch.ID, src.Label, ch.CreatedAt.Local().Format("2 Jan 15:04"), model, s.Importance, s.Text)
	if s.Importance == store.ImportanceCosmetic {
		fmt.Println("\n(cosmetic: this change would not be sent)")
	}
	return nil
}

func listChanges(ctx context.Context, st store.Store, limit int) error {
	changes, err := st.RecentChanges(ctx, 0, limit)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Println("No changes recorded yet.")
		return nil
	}
	labels := make(map[int64]string)
	for _, ch := range changes {
		label, ok := labels[ch.SourceID]
		if !ok {
			if src, err := st.Source(ctx, ch.SourceID); err == nil {
				label = src.Label
			}
			labels[ch.SourceID] = label
		}
		first, _, _ := strings.Cut(ch.Diff, "\n")
		if r := []rune(first); len(r) > 70 {
			first = string(r[:70]) + "…"
		}
		fmt.Printf("%6d  %s  %-10s  %-26s  %s\n", ch.ID, ch.CreatedAt.Local().Format("02 Jan 15:04"),
			ch.Status, label, first)
	}
	fmt.Println("\nRun `pehredaar summarize <id>` (add -lang hi for Hindi) to see one change's alert.")
	return nil
}
