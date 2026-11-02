// Command pehredaar watches exam notices and alerts subscribers on Telegram
// when they change.
//
// Usage:
//
//	pehredaar run [flags]           watch sources and serve the local dashboard
//	pehredaar probe [flags] <url>   fetch + normalize a URL and show the result
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	os.Exit(run())
}

// run returns the exit code. Keeping os.Exit out of it means deferred calls
// (like stop below) actually run; os.Exit skips defers.
func run() int {
	if len(os.Args) < 2 {
		usage()
		return 2
	}

	// Ctrl+C cancels ctx, which aborts in-flight requests and waits cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "run":
		err = runServe(ctx, args)
	case "probe":
		err = runProbe(ctx, args)
	case "help", "-h", "-help", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "pehredaar: unknown command %q\n\n", cmd)
		usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pehredaar:", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprint(os.Stderr, `Pehredaar: they changed it quietly. Pehredaar caught it.

Usage:
  pehredaar run [flags]           watch sources; dashboard at http://127.0.0.1:8787
  pehredaar probe [flags] <url>   fetch and normalize a URL, show what Pehredaar sees

Run "pehredaar <command> -h" for a command's flags.
`)
}
