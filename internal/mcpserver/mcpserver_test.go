package mcpserver

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jaspermayone/phial/internal/rank"
	"github.com/jaspermayone/phial/internal/store"
	"github.com/jaspermayone/phial/internal/tenant"
)

func connect(t *testing.T) (*mcp.ClientSession, *tenant.View) {
	t.Helper()
	m, err := tenant.New(t.TempDir(), rank.Default(), []tenant.Pool{
		{Name: "crane", Members: []string{"jsp"}, Subjects: []string{"areas/crane"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	v, err := m.View("jsp")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ct, stt := mcp.NewInMemoryTransports()
	if _, err := New(v).Connect(ctx, stt, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, v
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

var idRe = regexp.MustCompile(`ph_[0-9a-f]{24}`)

func TestToolsListed(t *testing.T) {
	cs, _ := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{"get_context", "recall", "outline", "read", "remember", "supersede", "retract", "confirm", "changes", "review"} {
		if !got[want] {
			t.Errorf("tool %s missing", want)
		}
	}
	if got["purge"] {
		t.Error("purge must not be an MCP tool")
	}
}

func TestRememberRecallSupersedeFlow(t *testing.T) {
	cs, v := connect(t)
	out, isErr := call(t, cs, "remember", map[string]any{
		"subject": "areas/phial", "text": "Deploys to alastor at phial.hogwarts.dev", "kind": "decision",
		"paths": []string{"deploy.target"}, "loci": []string{"nix/module.nix#services.phial"},
		"repo": "git@github.com:jaspermayone/phial.git", "description": "the memory server",
	})
	if isErr {
		t.Fatalf("remember failed: %s", out)
	}
	id := idRe.FindString(out)
	if id == "" {
		t.Fatalf("no id in %q", out)
	}
	p, err := v.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Loci) != 1 || p.Loci[0].Symbol != "services.phial" || len(p.Provenance) != 1 || p.Provenance[0].Repo != "github.com/jaspermayone/phial" || p.CreatedBy != "jsp" {
		t.Fatalf("stored phial = %+v", p)
	}

	out, _ = call(t, cs, "recall", map[string]any{"query": "where does it deploy", "explain": true})
	if !strings.Contains(out, id) || !strings.Contains(out, "score") || !strings.Contains(out, "by jsp") {
		t.Fatalf("recall did not find it with score and actor: %s", out)
	}

	out, isErr = call(t, cs, "supersede", map[string]any{"id": id, "text": "Deploys to alastor behind Traefik", "why": "proxy added"})
	if isErr {
		t.Fatalf("supersede: %s", out)
	}
	newID := idRe.FindAllString(out, -1)
	if len(newID) != 2 {
		t.Fatalf("supersede output: %s", out)
	}
	out, _ = call(t, cs, "recall", map[string]any{"query": "deploys alastor"})
	if strings.Contains(out, id) || !strings.Contains(out, newID[1]) {
		t.Fatalf("recall after supersede: %s", out)
	}
	out, _ = call(t, cs, "changes", map[string]any{"id": newID[1]})
	if !strings.Contains(out, "jsp · supersede") || !strings.Contains(out, "proxy added") || !strings.Contains(out, "{+behind Traefik+}") {
		t.Fatalf("changes: %s", out)
	}
	if out, isErr = call(t, cs, "retract", map[string]any{"id": newID[1]}); !isErr {
		t.Fatalf("retract without a reason should fail: %s", out)
	}
	if out, isErr = call(t, cs, "confirm", map[string]any{"id": newID[1]}); isErr {
		t.Fatalf("confirm: %s", out)
	}
}

func TestPoolsAndReview(t *testing.T) {
	cs, v := connect(t)
	ctx := context.Background()
	out, _ := call(t, cs, "remember", map[string]any{"subject": "areas/crane", "text": "The JS engine lives in crates/crane-js"})
	id := idRe.FindString(out)
	if !strings.Contains(out, "(shared: crane)") {
		t.Fatalf("pool label missing: %s", out)
	}
	if _, err := v.Propose(ctx, store.Proposal{TargetID: id, Text: "The JS engine lives in crates/crane-js, with tests in tests/js", Reason: "test", ProposedBy: "extract"}); err != nil {
		t.Fatal(err)
	}
	out, _ = call(t, cs, "review", map[string]any{})
	prID := regexp.MustCompile(`pr_[0-9a-f]{24}`).FindString(out)
	if prID == "" || !strings.Contains(out, "tests/js+}") {
		t.Fatalf("review list: %s", out)
	}
	out, isErr := call(t, cs, "review", map[string]any{"id": prID, "action": "apply"})
	if isErr || !strings.Contains(out, "Applied") {
		t.Fatalf("apply: %s", out)
	}
	out, _ = call(t, cs, "get_context", map[string]any{"project": "areas/crane"})
	if !strings.Contains(out, "shared pool") || !strings.Contains(out, "tests/js") {
		t.Fatalf("get_context for a pool: %s", out)
	}
}

func TestGetContext(t *testing.T) {
	cs, v := connect(t)
	ctx := context.Background()
	for _, in := range []store.RememberInput{
		{Subject: "profile", Text: "Name: Jasper"},
		{Subject: "topics/preferences", Kind: "preference", Text: "No em dashes."},
		{Subject: "areas/phial", Text: "Go and SQLite", Paths: []string{"stack"}, Provenance: store.Provenance{Repo: "github.com/jaspermayone/phial"}},
	} {
		if _, _, err := v.Remember(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	out, isErr := call(t, cs, "get_context", map[string]any{"gitRemote": "https://github.com/jaspermayone/phial.git", "path": "/x/phial"})
	if isErr {
		t.Fatal(out)
	}
	for _, want := range []string{"# Profile", "Name: Jasper", "# Project: areas/phial", "Go and SQLite", "# Subjects", "topics/preferences (1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("get_context missing %q:\n%s", want, out)
		}
	}
	out, _ = call(t, cs, "get_context", map[string]any{"path": "/tmp/nothing"})
	if !strings.Contains(out, "does not match an area") {
		t.Errorf("unknown workspace: %s", out)
	}
}

func TestReadAndOutline(t *testing.T) {
	cs, v := connect(t)
	ctx := context.Background()
	for _, in := range []store.RememberInput{
		{Subject: "areas/homelab", Text: "pve01 is an R630", Paths: []string{"proxmox.hardware"}},
		{Subject: "areas/homelab", Text: "VLAN 20 holds servers", Paths: []string{"network.vlans"}},
		{Subject: "areas/homelab/netbox", Text: "netbox runs on ollivander"},
	} {
		if _, _, err := v.Remember(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := call(t, cs, "read", map[string]any{"path": "areas/homelab.md"})
	if !strings.Contains(out, "## network") || !strings.Contains(out, "## proxmox") || strings.Contains(out, "ollivander") {
		t.Fatalf("read output:\n%s", out)
	}
	out, _ = call(t, cs, "outline", map[string]any{"subject": "areas/homelab", "depth": 3})
	if !strings.Contains(out, "network.vlans (1)") || !strings.Contains(out, "(no path) (1)") {
		t.Fatalf("outline output:\n%s", out)
	}
	if out, isErr := call(t, cs, "read", map[string]any{"path": "areas/missing"}); !isErr {
		t.Fatalf("read of a missing subject should fail: %s", out)
	}
}
