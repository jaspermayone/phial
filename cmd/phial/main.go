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
	"github.com/jaspermayone/phial/internal/extract"
	"github.com/jaspermayone/phial/internal/httpapi"
	"github.com/jaspermayone/phial/internal/importer"
	"github.com/jaspermayone/phial/internal/llm"
	"github.com/jaspermayone/phial/internal/mcpserver"
	"github.com/jaspermayone/phial/internal/store"
	"github.com/jaspermayone/phial/internal/tenant"
)

const usage = `phial: a memory server for coding agents

Usage:
  phial serve                          run the server (MCP at /mcp, UI at /)
  phial import lard --dir DIR          import a lard memory dir or backup tenant
  phial import lard --url URL          import from a running lard server
                    [--token-file F]   bearer token for --url (or LARD_TOKEN)
                    [--user ID]        the identity to import into (default: primary_user)
                    [--apply]          write; without it the import is a dry run
  phial extract [--user ID] [--retry-failed]
                                       run one extraction pass now; --retry-failed first
                                       requeues sessions whose last extraction failed
  phial review-triage [--user ID] [--apply]
                                       re-sort pending proposals: same text confirms the
                                       phial, a sibling fact becomes a new phial
  phial users                          list identities that have a store
  phial rank-report [--user ID] [--limit N]
  phial purge ID --reason TEXT [--user ID]
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
	case "extract":
		err = extractCmd(ctx, os.Args[2:])
	case "review-triage":
		err = triageCmd(ctx, os.Args[2:])
	case "users":
		err = users()
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

func manager() (*config.Config, *tenant.Manager, error) {
	cfg, err := config.LoadAdmin()
	if err != nil {
		return nil, nil, err
	}
	m, err := tenant.New(cfg.DataDir, cfg.Ranking, cfg.Pools, cfg.Auth.AllowedUsers)
	if err != nil {
		return nil, nil, err
	}
	return cfg, m, nil
}

func userFlag(fs *flag.FlagSet) *string {
	return fs.String("user", "", "identity to act as (default: primary_user)")
}

func viewFor(cfg *config.Config, m *tenant.Manager, user string) (*tenant.View, error) {
	if user == "" {
		user = cfg.PrimaryUser
	}
	return m.View(user)
}

func serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m, err := tenant.New(cfg.DataDir, cfg.Ranking, cfg.Pools, cfg.Auth.AllowedUsers)
	if err != nil {
		return err
	}
	defer m.Close()

	token := cfg.Auth.Token
	if cfg.Auth.Mode == "none" {
		token = ""
		slog.Warn("auth is off (mode none); only use this on a loopback address for testing")
	}
	a := auth.New(token, cfg.SecureCookies())
	a.Origin = auth.OriginOf(cfg.PublicURL)
	a.Primary = cfg.PrimaryUser
	var oa *auth.OAuth
	if cfg.Auth.Mode == "oauth" {
		oa = &auth.OAuth{
			Issuer: cfg.Auth.Issuer, PublicURL: cfg.PublicURL, IntrospectionURL: cfg.Auth.IntrospectionURL,
			ClientID: cfg.Auth.ClientID, ClientSecret: cfg.Auth.ClientSecret,
			AcceptedAudiences: cfg.Auth.AcceptedAudiences, RequiredScopes: cfg.Auth.RequiredScopes,
			Allowed: m.Allowed,
		}
	}
	h, err := httpapi.New(httpapi.Options{Manager: m, Auth: a, OAuth: oa, Mode: cfg.Auth.Mode, Primary: cfg.PrimaryUser, UILogin: cfg.Auth.UILogin})
	if err != nil {
		return err
	}
	client := &llm.Client{Provider: cfg.LLM.Provider, BaseURL: cfg.LLM.BaseURL, Model: cfg.LLM.Model, APIKey: cfg.LLM.APIKey}
	worker := &extract.Worker{Manager: m, Client: client, Interval: time.Duration(cfg.Extract.Interval), Quiet: time.Duration(cfg.Extract.Quiet)}
	go worker.Run(ctx)

	srv := &http.Server{Addr: cfg.Addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("phial listening", "addr", cfg.Addr, "data", cfg.DataDir, "auth", cfg.Auth.Mode, "ui_login", cfg.Auth.UILogin,
		"pools", len(cfg.Pools), "extract", client.Enabled(), "version", mcpserver.Version)
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
		return errors.New("usage: phial import lard (--dir DIR | --url URL) [--user ID] [--apply]")
	}
	fs := flag.NewFlagSet("import lard", flag.ContinueOnError)
	dir := fs.String("dir", "", "lard memory directory, or a backup tenant that holds memory/")
	url := fs.String("url", "", "base URL of a running lard server")
	tokenFile := fs.String("token-file", "", "file with a bearer token for --url (default: $LARD_TOKEN)")
	user := userFlag(fs)
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

	cfg, m, err := manager()
	if err != nil {
		return err
	}
	defer m.Close()
	// A dry run on a new identity would create its store; that is a
	// directory and an empty database, nothing more, so it is fine.
	v, err := viewFor(cfg, m, *user)
	if err != nil {
		return err
	}
	rep, err := importer.Import(ctx, v, subs, !*apply)
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
	fmt.Printf("\n%s %d item(s) from %d subject(s) into %s: %d created, %d already present, %d subject(s) failed.\n",
		mode, rep.Items, len(rep.Subjects), v.Identity, rep.Created, rep.Existing, rep.Failed)
	if rep.DryRun {
		fmt.Println("Run again with --apply to write.")
	}
	return nil
}

func extractCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	user := userFlag(fs)
	retry := fs.Bool("retry-failed", false, "requeue sessions whose last extraction failed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, m, err := manager()
	if err != nil {
		return err
	}
	defer m.Close()
	client := &llm.Client{Provider: cfg.LLM.Provider, BaseURL: cfg.LLM.BaseURL, Model: cfg.LLM.Model, APIKey: cfg.LLM.APIKey}
	if !client.Enabled() {
		return errors.New("no LLM configured: set llm.model and llm.api_key (PHIAL_LLM_MODEL, PHIAL_LLM_API_KEY); provider anthropic needs no base_url")
	}
	v, err := viewFor(cfg, m, *user)
	if err != nil {
		return err
	}
	if *retry {
		n, err := v.Own.RequeueFailedSessions(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%d failed session(s) requeued\n", n)
	}
	pending, err := v.Own.PendingSessions(ctx, 500)
	if err != nil {
		return err
	}
	fmt.Printf("%d session(s) pending for %s\n", len(pending), v.Identity)
	for _, se := range pending {
		res, err := extract.Session(ctx, client, v, se)
		errText := ""
		if err != nil {
			errText = err.Error()
			fmt.Printf("  %s: FAILED: %v\n", se.ID, err)
		} else {
			fmt.Printf("  %s: %d candidates, %d created, %d confirmed, %d proposed, %d dropped\n", se.ID, res.Candidates, res.Created, res.Confirmed, res.Proposed, res.Dropped)
		}
		if err := v.Own.MarkExtracted(ctx, se.ID, errText); err != nil {
			return err
		}
	}
	return nil
}

func users() error {
	_, m, err := manager()
	if err != nil {
		return err
	}
	defer m.Close()
	ids, err := m.Identities()
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Printf("%s\t%s\n", id, m.UserDir(id))
	}
	for _, p := range m.Pools() {
		fmt.Printf("pool %s\tmembers=%s readers=%s subjects=%s\n", p.Name, strings.Join(p.Members, ","), strings.Join(p.Readers, ","), strings.Join(p.Subjects, ","))
	}
	return nil
}

func rankReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rank-report", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "how many phials to list")
	user := userFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, m, err := manager()
	if err != nil {
		return err
	}
	defer m.Close()
	v, err := viewFor(cfg, m, *user)
	if err != nil {
		return err
	}
	rs, err := v.Recall(ctx, store.Query{Lowest: true, Limit: *limit})
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
		sub := r.Subject
		if r.Pool != "" {
			sub += " (" + r.Pool + ")"
		}
		fmt.Fprintf(tw, "%.2f\t%.2f\t%.2f\t%s\t%s\t%s\n", r.Rank.Score, r.Rank.Freshness, r.Rank.Trust, r.ID, sub, text)
	}
	return tw.Flush()
}

func purge(ctx context.Context, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: phial purge ID --reason TEXT [--user ID]")
	}
	id := args[0]
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	reason := fs.String("reason", "", "why this phial must not exist (required; logged)")
	user := userFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, m, err := manager()
	if err != nil {
		return err
	}
	defer m.Close()
	v, err := viewFor(cfg, m, *user)
	if err != nil {
		return err
	}
	if err := v.Purge(ctx, id, *reason); err != nil {
		return err
	}
	fmt.Printf("Purged %s. The audit line has the ID, the time and the reason, not the text.\n", id)
	return nil
}

func triageCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review-triage", flag.ContinueOnError)
	user := userFlag(fs)
	apply := fs.Bool("apply", false, "resolve the proposals; without it only count")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, m, err := manager()
	if err != nil {
		return err
	}
	defer m.Close()
	v, err := viewFor(cfg, m, *user)
	if err != nil {
		return err
	}
	res, err := extract.Triage(ctx, v, *apply, func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) })
	if err != nil {
		return err
	}
	mode := "would be"
	if *apply {
		mode = "were"
	}
	fmt.Printf("%d pending: %d kept as proposals, %d %s rejected and their phial confirmed, %d %s remembered as new phials, %d skipped\n",
		res.Pending, res.Kept, res.Confirmed, mode, res.New, mode, res.Skipped)
	return nil
}
