// Command phial runs the phial memory server and its admin tasks.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jaspermayone/phial/internal/auth"
	"github.com/jaspermayone/phial/internal/config"
	"github.com/jaspermayone/phial/internal/httpapi"
	"github.com/jaspermayone/phial/internal/importer"
	"github.com/jaspermayone/phial/internal/mcpserver"
	"github.com/jaspermayone/phial/internal/store"
)

const usage = `phial: a memory server for coding agents

Usage:
  phial serve                          run the server (MCP at /mcp, UI at /)
  phial import lard --dir DIR          import a lard memory dir or backup tenant
  phial import lard --url URL          import from a running lard server
                    [--token-file F]   bearer token for --url (or LARD_TOKEN)
                    [--apply]          write; without it the import is a dry run
  phial rank-report [--limit N]        active phials with the lowest scores
  phial purge ID --reason TEXT         remove one phial for good (writes an audit line)
  phial version

Config: $PHIAL_CONFIG or ~/.config/phial/config.toml; env vars win.
See docs/design.md.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx)
	case "import":
		err = importCmd(ctx, os.Args[2:])
	case "rank-report":
		err = rankReport(ctx, os.Args[2:])
	case "purge":
		err = purge(ctx, os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("phial", mcpserver.Version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "phial: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "phial:", err)
		os.Exit(1)
	}
}

func openStore() (*config.Config, *store.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.DB, cfg.Ranking)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", cfg.DB, err)
	}
	return cfg, st, nil
}

func serve(ctx context.Context) error {
	cfg, st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	token := cfg.Auth.Token
	if cfg.Auth.Mode == "none" {
		token = ""
		slog.Warn("auth is off (mode none); only use this on a loopback address for testing")
	}
	a := auth.New(token, cfg.SecureCookies())
	a.Origin = auth.OriginOf(cfg.PublicURL)
	h, err := httpapi.New(st, a)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("phial listening", "addr", cfg.Addr, "db", cfg.DB, "auth", cfg.Auth.Mode, "token", a.Fingerprint(), "version", mcpserver.Version)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shut)
}

func importCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "lard" {
		return errors.New("usage: phial import lard (--dir DIR | --url URL) [--apply]")
	}
	fs := flag.NewFlagSet("import lard", flag.ContinueOnError)
	dir := fs.String("dir", "", "lard memory directory, or a backup tenant that holds memory/")
	url := fs.String("url", "", "base URL of a running lard server")
	tokenFile := fs.String("token-file", "", "file with a bearer token for --url (default: $LARD_TOKEN)")
	apply := fs.Bool("apply", false, "write the phials; without it this is a dry run")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if (*dir == "") == (*url == "") {
		return errors.New("give exactly one of --dir or --url")
	}

	var subs []importer.LardSubject
	var err error
	if *dir != "" {
		subs, err = importer.ReadDir(*dir)
	} else {
		token := os.Getenv("LARD_TOKEN")
		if *tokenFile != "" {
			b, ferr := os.ReadFile(*tokenFile)
			if ferr != nil {
				return ferr
			}
			token = strings.TrimSpace(string(b))
		}
		subs, err = importer.Fetch(ctx, *url, token, nil)
	}
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// A dry run writes nothing, so it does not create a database that is
	// not there yet; it then cannot count what already exists.
	var st *store.Store
	if _, statErr := os.Stat(cfg.DB); *apply || statErr == nil {
		if st, err = store.Open(cfg.DB, cfg.Ranking); err != nil {
			return fmt.Errorf("open %s: %w", cfg.DB, err)
		}
		defer st.Close()
	}
	rep, err := importer.Import(ctx, st, subs, !*apply)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SUBJECT\tITEMS\tCREATED\tEXISTING\tERROR")
	for _, s := range rep.Subjects {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\n", s.Subject, s.Items, s.Created, s.Existing, s.Error)
	}
	tw.Flush()
	mode := "Imported"
	if rep.DryRun {
		mode = "Dry run, nothing written. Would import"
	}
	fmt.Printf("\n%s %d item(s) from %d subject(s): %d created, %d already present, %d subject(s) failed.\n",
		mode, rep.Items, len(rep.Subjects), rep.Created, rep.Existing, rep.Failed)
	if rep.DryRun {
		fmt.Println("Run again with --apply to write.")
	}
	return nil
}

func rankReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rank-report", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "how many phials to list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	rs, err := st.Recall(ctx, store.Query{Lowest: true, Limit: *limit})
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SCORE\tFRESH\tTRUST\tID\tSUBJECT\tTEXT")
	for _, r := range rs {
		text := r.Text
		if len(text) > 70 {
			text = text[:70] + "…"
		}
		fmt.Fprintf(tw, "%.2f\t%.2f\t%.2f\t%s\t%s\t%s\n", r.Rank.Score, r.Rank.Freshness, r.Rank.Trust, r.ID, r.Subject, text)
	}
	return tw.Flush()
}

func purge(ctx context.Context, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: phial purge ID --reason TEXT")
	}
	id := args[0]
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	reason := fs.String("reason", "", "why this phial must not exist (required; logged)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	_, st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Purge(ctx, id, *reason); err != nil {
		return err
	}
	fmt.Printf("Purged %s. The audit line has the ID, the time and the reason, not the text.\n", id)
	return nil
}
