// Command phial-client logs a machine in to phial, uploads agent sessions,
// checks code anchors, and serves brief and verify to a local agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jaspermayone/phial/internal/client"
	"github.com/jaspermayone/phial/internal/store"
)

var version = "dev"

const usage = `phial-client: the machine side of phial

Usage:
  phial-client login --url URL [--token T] [--root DIR...]   device-code login (or a static token)
  phial-client logout
  phial-client status
  phial-client sync [--all] [--quiet]        upload new Claude Code sessions and entire-brain memories
  phial-client daemon [--interval 5m]        sync in a loop
  phial-client service install|uninstall|status
  phial-client verify [DIR]                  check this repo's code anchors
  phial-client brief "task" [--dir DIR]      context for a task
  phial-client remember --subject S "text"   store one phial (--kind, --path, --locus repeatable)
  phial-client recall "words" [--subject S]  search phials
  phial-client mcp                           local MCP server (stdio) with brief and verify
  phial-client version

OAuth client: set PHIAL_CLIENT_ID to the client the server's operator
registered (a public client with PKCE and device code needs no secret;
PHIAL_CLIENT_SECRET is only for a confidential one), and PHIAL_CLIENT_SCOPES
to change the scopes.
Config: ~/.config/phial/client.toml (0600).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, os.Args[1], os.Args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "phial-client:", err)
		os.Exit(1)
	}
}

func logf(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }

func run(ctx context.Context, cmd string, args []string) error {
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("phial-client", version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	}
	cfg, err := client.Load()
	if err != nil {
		return err
	}
	switch cmd {
	case "login":
		fs := flag.NewFlagSet("login", flag.ContinueOnError)
		url := fs.String("url", cfg.Server, "the phial server")
		token := fs.String("token", "", "a static bearer token instead of OAuth")
		var roots multi
		fs.Var(&roots, "root", "directory whose repos are synced (repeatable)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *url == "" {
			return errors.New("--url is required the first time")
		}
		cfg.Server = strings.TrimRight(*url, "/")
		if len(roots) > 0 {
			cfg.Roots = roots
		}
		if *token != "" {
			cfg.Auth, cfg.Token = "token", *token
			if err := cfg.Save(); err != nil {
				return err
			}
		} else if err := client.LoginDevice(ctx, cfg); err != nil {
			return err
		}
		who, err := client.NewAPI(cfg).Whoami(ctx)
		if err != nil {
			return fmt.Errorf("logged in, but the server refused the first call: %w", err)
		}
		fmt.Printf("Logged in to %s as %s.\n", cfg.Server, who.Identity)
		if len(cfg.Roots) == 0 {
			fmt.Println("No roots set: every Claude Code session on this machine will be synced. Use --root to limit it.")
		}
		return nil
	case "logout":
		return cfg.Logout()
	}
	if !cfg.LoggedIn() {
		return errors.New("not logged in; run: phial-client login --url https://phial.hogwarts.dev")
	}
	api := client.NewAPI(cfg)
	switch cmd {
	case "status":
		who, err := api.Whoami(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("server    %s (%s)\n", cfg.Server, who.Version)
		fmt.Printf("identity  %s\n", who.Identity)
		if len(who.Pools) > 0 {
			fmt.Printf("pools     %s\n", strings.Join(who.Pools, ", "))
		}
		fmt.Printf("phials    %d active, %d superseded, %d retracted\n", who.Stats.Active, who.Stats.Superseded, who.Stats.Retracted)
		fmt.Printf("sessions  %d uploaded, %d waiting for extraction\n", who.Sessions, who.PendingSessions)
		fmt.Printf("roots     %s\n", strings.Join(cfg.Roots, ", "))
		fmt.Printf("synced    %d transcript(s) seen\n", len(cfg.Synced))
		fmt.Printf("service   %s\n", client.ServiceStatus())
		return nil
	case "sync":
		fs := flag.NewFlagSet("sync", flag.ContinueOnError)
		all := fs.Bool("all", false, "read every transcript again")
		quiet := fs.Bool("quiet", false, "print only when something was sent")
		if err := fs.Parse(args); err != nil {
			return err
		}
		rep, err := client.Sync(ctx, cfg, api, *all, logf)
		if err != nil {
			return err
		}
		if !*quiet || rep.New > 0 || rep.Brain > 0 {
			fmt.Printf("%s: %d transcript(s), %d session(s) read, %d new on the server, %d skipped, %d entire-brain memories sent\n",
				time.Now().Format("2006-01-02 15:04"), rep.Files, rep.Sessions, rep.New, rep.Skipped, rep.Brain)
		}
		return nil
	case "daemon":
		fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
		interval := fs.Duration("interval", 5*time.Minute, "time between syncs")
		if err := fs.Parse(args); err != nil {
			return err
		}
		for {
			if rep, err := client.Sync(ctx, cfg, api, false, logf); err != nil {
				logf("sync: %v", err)
			} else if rep.New > 0 || rep.Brain > 0 {
				logf("sync: %d new session(s), %d brain memories", rep.New, rep.Brain)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(*interval):
			}
		}
	case "service":
		if len(args) == 0 {
			return errors.New("usage: phial-client service install|uninstall|status")
		}
		switch args[0] {
		case "install":
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			path, err := client.ServiceInstall(exe, 5)
			if err != nil {
				return err
			}
			fmt.Println("Installed:", path)
			return nil
		case "uninstall":
			return client.ServiceUninstall()
		case "status":
			fmt.Println(client.ServiceStatus())
			return nil
		}
		return errors.New("usage: phial-client service install|uninstall|status")
	case "verify":
		dir := "."
		if len(args) > 0 {
			dir = args[0]
		}
		checked, changed, err := client.VerifyRepo(ctx, api, dir, logf)
		if err != nil {
			return err
		}
		fmt.Printf("Checked %d phial(s); %d changed state.\n", checked, changed)
		return nil
	case "brief":
		fs := flag.NewFlagSet("brief", flag.ContinueOnError)
		dir := fs.String("dir", ".", "the checkout")
		if err := fs.Parse(args); err != nil {
			return err
		}
		task := strings.Join(fs.Args(), " ")
		if task == "" {
			return errors.New("usage: phial-client brief \"what you are about to do\"")
		}
		out, err := client.Brief(ctx, api, *dir, task)
		if err != nil {
			return err
		}
		fmt.Print(out)
		return nil
	case "remember":
		fs := flag.NewFlagSet("remember", flag.ContinueOnError)
		subject := fs.String("subject", "", "profile, areas/<name>, topics/<name> or people/<name>")
		kind := fs.String("kind", "fact", "decision, invariant, gotcha, preference, convention or fact")
		desc := fs.String("description", "", "subject description, only when this makes a new subject")
		var paths, lociArgs multi
		fs.Var(&paths, "path", "dotted path (repeatable)")
		fs.Var(&lociArgs, "locus", "code anchor file#symbol (repeatable)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		text := strings.Join(fs.Args(), " ")
		if *subject == "" || strings.TrimSpace(text) == "" {
			return errors.New("usage: phial-client remember --subject areas/<name> [--kind K] [--path P] \"one durable statement\"")
		}
		var loci []store.Locus
		for _, l := range lociArgs {
			loci = append(loci, store.ParseLocus(l))
		}
		dir, _ := os.Getwd()
		p, created, err := api.Remember(ctx, store.RememberInput{
			Subject: *subject, Kind: *kind, Text: text, Paths: paths, Loci: loci, Description: *desc,
			Provenance: store.Provenance{Repo: client.GitRemote(dir), Source: "phial-client on " + client.Hostname()},
		})
		if err != nil {
			return err
		}
		verb := "Remembered"
		if !created {
			verb = "Already known; confirmed"
		}
		fmt.Printf("%s %s in %s\n", verb, p.ID, p.Subject)
		return nil
	case "recall":
		fs := flag.NewFlagSet("recall", flag.ContinueOnError)
		subject := fs.String("subject", "", "limit to a subject")
		kind := fs.String("kind", "", "limit to a kind")
		limit := fs.Int("limit", 10, "maximum results")
		if err := fs.Parse(args); err != nil {
			return err
		}
		rs, err := api.Recall(ctx, store.Query{Text: strings.Join(fs.Args(), " "), Subject: *subject, Kind: *kind, Limit: *limit})
		if err != nil {
			return err
		}
		for _, r := range rs {
			pool := ""
			if r.Pool != "" {
				pool = " (shared: " + r.Pool + ")"
			}
			fmt.Printf("- [%s] (%s) %s · %s%s\n", r.ID, r.Kind, r.Text, r.Subject, pool)
		}
		if len(rs) == 0 {
			fmt.Println("No phials match.")
		}
		return nil
	case "mcp":
		return client.ServeMCP(ctx, api, version)
	}
	return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }
