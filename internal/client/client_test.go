package client

import (
	"os"
	"path/filepath"
	"testing"
)

const sample = `{"type":"mode","mode":"normal","sessionId":"abc"}
{"type":"user","sessionId":"abc","cwd":"/x/repo","timestamp":"2026-10-31T10:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"deploy phial to alastor"},{"type":"image","source":{}}]}}
{"type":"assistant","sessionId":"abc","cwd":"/x/repo","timestamp":"2026-10-31T10:00:05.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"On it."}]}}
{"type":"user","sessionId":"abc","isMeta":true,"message":{"role":"user","content":"meta line"}}
{"type":"user","sessionId":"abc","isSidechain":true,"message":{"role":"user","content":"subagent prompt"}}
{"type":"user","sessionId":"abc","timestamp":"2026-10-31T10:01:00.000Z","message":{"role":"user","content":[{"type":"tool_result","content":"tool output"}]}}
{"type":"user","sessionId":"abc","timestamp":"2026-10-31T10:02:00.000Z","message":{"role":"user","content":"<command-name>/help</command-name>"}}
{"type":"user","sessionId":"abc","timestamp":"2026-10-31T10:03:00.000Z","message":{"role":"user","content":"use port 7480"}}
`

func TestParseTranscript(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, []byte(sample), 0o600)
	tr, err := ParseTranscript(p)
	if err != nil {
		t.Fatal(err)
	}
	if tr == nil {
		t.Fatal("nil transcript")
	}
	se := tr.Session
	if se.ID != "claude-code:abc" || se.Path != "/x/repo" || se.Source != "claude-code" {
		t.Fatalf("session = %+v", se)
	}
	if len(se.Turns) != 3 || se.Turns[0].Text != "deploy phial to alastor" || se.Turns[1].Role != "assistant" || se.Turns[2].Text != "use port 7480" {
		t.Fatalf("turns = %+v", se.Turns)
	}
	if se.StartedAt.IsZero() || !se.EndedAt.After(se.StartedAt) {
		t.Fatalf("times = %v %v", se.StartedAt, se.EndedAt)
	}
	empty := filepath.Join(t.TempDir(), "e.jsonl")
	os.WriteFile(empty, []byte(`{"type":"assistant","sessionId":"z","message":{"role":"assistant","content":"hi"}}`+"\n"), 0o600)
	if tr, _ := ParseTranscript(empty); tr != nil {
		t.Fatal("a session with no user prompt should be nil")
	}
}

func TestParseBrainFile(t *testing.T) {
	m, ok := ParseBrainFile("---\nid: fact:12dc\nkind: invariant\npaths: constraints.retry.policy\nbranch: main\nstatus: active\nanchor: commit 53e3d58\n---\n\nRetry failed requests at most three times.\n")
	if !ok || m.ID != "fact:12dc" || m.Kind != "invariant" || m.Paths[0] != "constraints.retry.policy" || m.Commit != "53e3d58" || m.Text != "Retry failed requests at most three times." {
		t.Fatalf("parsed = %+v ok=%v", m, ok)
	}
	if brainPath("constraints.retry.policy") != "retry.policy" || brainPath("project.deploy") != "project.deploy" {
		t.Fatal("brainPath")
	}
}

func TestUnderRoots(t *testing.T) {
	if !underRoots("/a/b/c", nil) || !underRoots("/a/b/c", []string{"/a/b"}) || underRoots("/a/bc", []string{"/a/b"}) {
		t.Fatal("underRoots")
	}
}
