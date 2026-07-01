package codex

import (
	"context"
	convlogic "github.com/agentcarto/core/conversation"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseNormalizesRolesAndMetaTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"text":"rules"}]}}
{"timestamp":"2025-01-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"hello"}]}}
{"timestamp":"2025-01-01T00:00:03Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"run"}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	ev, cwd, id, _, _, _ := parse(context.Background(), p)
	if cwd != "/work" || id != "id" {
		t.Fatalf("%q %q", cwd, id)
	}
	if ev[0].Kind != domain.EventSystem || ev[1].Kind != domain.EventUser || ev[2].Kind != domain.EventToolCall {
		t.Fatalf("%#v", ev)
	}
}
func TestRollbackConversationKeepsDeadBranch(t *testing.T) {
	ev := []domain.Event{{Kind: domain.EventUser, Text: "q1"}, {Kind: domain.EventAssistant, Text: "a1"}, {Kind: domain.EventUser, Text: "q2"}, {Kind: domain.EventAssistant, Text: "a2"}, {Kind: domain.EventMeta, Text: "1", RawType: "thread_rolled_back"}, {Kind: domain.EventUser, Text: "q2'"}}
	c := rollbackConversation(ev, "thread_rolled_back")
	var users []string
	active := map[string]bool{}
	for _, id := range c.ActivePath() {
		active[id] = true
		for _, e := range c.Nodes[id].Events {
			if e.Kind == domain.EventUser {
				users = append(users, e.Text)
			}
		}
	}
	if len(users) != 2 || users[1] != "q2'" {
		t.Fatalf("%v", users)
	}
	dead := false
	for id, n := range c.Nodes {
		if !active[id] && len(n.Events) > 0 && n.Events[0].Text == "q2" {
			dead = true
		}
	}
	if !dead {
		t.Fatal("dead branch missing")
	}
}

func TestRollbackIgnoresCodexPreambleAsTurn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"<environment_context> cwd=/x </environment_context>"}]}}
{"timestamp":"2025-01-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"test1"}]}}
{"timestamp":"2025-01-01T00:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"a1"}]}}
{"timestamp":"2025-01-01T00:00:04Z","type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":1}}
{"timestamp":"2025-01-01T00:00:05Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"test1'"}]}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	pl := &Plugin{}
	c, err := pl.LoadConversation(context.Background(), domain.SessionRef{Source: p})
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, id := range c.ActivePath() {
		for _, e := range c.Nodes[id].Events {
			if e.Kind == domain.EventUser {
				users = append(users, e.Text)
			}
		}
	}
	if strings.Join(users, ",") != "test1'" {
		t.Fatalf("users=%v active=%v nodes=%#v", users, c.ActivePath(), c.Nodes)
	}
}
func TestLoadConversationKeepsSameCodexTurnIDTogether(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"Q1"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}
{"timestamp":"2025-01-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"working"}],"phase":"commentary","internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}
{"timestamp":"2025-01-01T00:00:03Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"follow-up while running"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}
{"timestamp":"2025-01-01T00:00:04Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"still working"}],"phase":"commentary","internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}
{"timestamp":"2025-01-01T00:00:05Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"Q2"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-2"}}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	pl := &Plugin{}
	c, err := pl.LoadConversation(context.Background(), domain.SessionRef{Source: p})
	if err != nil {
		t.Fatal(err)
	}
	turns := convlogic.TurnsOfPath(*c, c.ActivePath())
	if len(turns) != 2 {
		t.Fatalf("turns=%v nodes=%#v", turns, c.Nodes)
	}
	events := 0
	queued := 0
	for _, id := range turns[0] {
		events += len(c.Nodes[id].Events)
		for _, e := range c.Nodes[id].Events {
			if e.Kind == domain.EventQueued {
				queued++
			}
		}
	}
	if events != 4 || queued != 1 {
		t.Fatalf("same turn_id events should stay in first turn with queued marker: events=%d queued=%d turns=%v", events, queued, turns)
	}
}
func TestLoadConversationQueuesUserAfterTurnAbort(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"Q1"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}
{"timestamp":"2025-01-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"working"}],"phase":"commentary","internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}
{"timestamp":"2025-01-01T00:00:03Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"<turn_aborted>\nThe user interrupted the previous turn on purpose.\n</turn_aborted>"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}
{"timestamp":"2025-01-01T00:00:03Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-1"}}
{"timestamp":"2025-01-01T00:00:04Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"queued after abort"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-2"}}}
{"timestamp":"2025-01-01T00:00:05Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"handled queued input"}],"phase":"commentary","internal_chat_message_metadata_passthrough":{"turn_id":"turn-2"}}}
{"timestamp":"2025-01-01T00:00:06Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"Q2"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-3"}}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	pl := &Plugin{}
	c, err := pl.LoadConversation(context.Background(), domain.SessionRef{Source: p})
	if err != nil {
		t.Fatal(err)
	}
	turns := convlogic.TurnsOfPath(*c, c.ActivePath())
	if len(turns) != 2 {
		t.Fatalf("turns=%v nodes=%#v", turns, c.Nodes)
	}
	queued := 0
	for _, id := range turns[0] {
		for _, e := range c.Nodes[id].Events {
			if e.Kind == domain.EventQueued && e.Text == "queued after abort" && e.TurnID == "turn-1" {
				queued++
			}
		}
	}
	if queued != 1 {
		t.Fatalf("queued user after abort should attach to aborted turn: queued=%d turns=%v nodes=%#v", queued, turns, c.Nodes)
	}
}
func TestRollbackCountsSameCodexTurnIDOnce(t *testing.T) {
	ev := []domain.Event{
		{Kind: domain.EventUser, Text: "Q1", TurnID: "turn-1"},
		{Kind: domain.EventAssistant, Text: "working", TurnID: "turn-1"},
		{Kind: domain.EventUser, Text: "follow-up while running", TurnID: "turn-1"},
		{Kind: domain.EventAssistant, Text: "A1", TurnID: "turn-1"},
		{Kind: domain.EventMeta, Text: "1", RawType: "thread_rolled_back"},
		{Kind: domain.EventUser, Text: "Q2", TurnID: "turn-2"},
	}
	c := rollbackConversation(ev, "thread_rolled_back")
	var users []string
	for _, id := range c.ActivePath() {
		for _, e := range c.Nodes[id].Events {
			if e.Kind == domain.EventUser {
				users = append(users, e.Text)
			}
		}
	}
	if strings.Join(users, ",") != "Q2" {
		t.Fatalf("users=%v active=%v nodes=%#v", users, c.ActivePath(), c.Nodes)
	}
}
func TestPatchDocumentRendersApplyPatch(t *testing.T) {
	got := patchDocument(map[string]any{
		"a.go": map[string]any{"type": "update", "unified_diff": "--- a/a.go\n+++ b/a.go\n+new\n-old\n"},
		"b.go": map[string]any{"type": "add", "content": "one\ntwo\n"},
	})
	for _, want := range []string{
		"*** Begin Patch",
		"*** Update File: a.go",
		"+new",
		"-old",
		"*** Add File: b.go",
		"+one",
		"+two",
		"*** End Patch",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}
func TestParseIgnoresCodexEventMsgConversationDuplicates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"hello"}]}}
{"timestamp":"2025-01-01T00:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"hello"}}
{"timestamp":"2025-01-01T00:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"hi"}]}}
{"timestamp":"2025-01-01T00:00:04Z","type":"event_msg","payload":{"type":"agent_message","message":"hi"}}
{"timestamp":"2025-01-01T00:00:05Z","type":"event_msg","payload":{"type":"turn_ended"}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	ev, _, _, _, _, _ := parse(context.Background(), p)
	users, assistants := 0, 0
	for _, e := range ev {
		if e.Kind == domain.EventUser {
			users++
		}
		if e.Kind == domain.EventAssistant {
			assistants++
		}
		if e.Kind == domain.EventTurnComplete && e.RawType == "turn_ended" {
			t.Fatalf("event_msg turn_ended should not be conversation event: %#v", ev)
		}
	}
	if users != 1 || assistants != 1 {
		t.Fatalf("duplicates were not filtered: users=%d assistants=%d events=%#v", users, assistants, ev)
	}
}
func TestCodexOutputTextStripsChunkMetadata(t *testing.T) {
	out := codexOutputText([]any{
		map[string]any{"type": "input_text", "text": "Script completed\nWall time 0.1 seconds\nOutput:\n"},
		map[string]any{"type": "input_text", "text": `{"chunk_id":"abc","wall_time_seconds":0.1,"exit_code":0,"original_token_count":2,"output":"real output\n"}`},
	})
	if strings.Contains(out, "chunk_id") || strings.Contains(out, "Script completed") || strings.Contains(out, "Wall time") {
		t.Fatalf("metadata leaked: %q", out)
	}
	if out != "real output" {
		t.Fatalf("unexpected output: %q", out)
	}
}
func TestParseCodexCompactedAsCompactSummary(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"hello"}]}}
{"timestamp":"2025-01-01T00:00:02Z","type":"compacted","payload":{"message":"summary"}}
{"timestamp":"2025-01-01T00:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"after"}]}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	ev, _, _, _, _, _ := parse(context.Background(), p)
	found := false
	for _, e := range ev {
		if e.RawType == "compact_summary" && e.Kind == domain.EventUser {
			found = true
		}
	}
	if !found {
		t.Fatalf("compact_summary missing: %#v", ev)
	}
}

func TestScanCodexForkParentMetadata(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "06", "23")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(day, "rollout-2026-06-23T00-00-00-child.jsonl")
	data := `{"timestamp":"2026-06-23T00:00:00Z","type":"session_meta","payload":{"id":"child","cwd":"/work","forked_from_id":"parent"}}
{"timestamp":"2026-06-23T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"fork"}]}}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	p := &Plugin{id: "codex", o: Options{SessionsDir: root}}
	res, err := p.Scan(context.Background(), plugin.ScanInput{})
	if err != nil {
		t.Fatal(err)
	}
	ss := res.Sessions
	if len(ss) != 1 || ss[0].ParentSessionID != "parent" {
		t.Fatalf("sessions=%#v", ss)
	}
}

func TestParseCodexAssistantEmptyMessageIsTurnCompleteTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[]}}
{"timestamp":"2025-01-01T00:00:02Z","type":"event_msg","payload":{"type":"token_count"}}
{"timestamp":"2025-01-01T00:00:03Z","type":"event_msg","payload":{"type":"task_complete"}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	ev, _, _, _, _, _ := parse(context.Background(), p)
	if got := ev[len(ev)-1].Kind; got != domain.EventTurnComplete {
		t.Fatalf("last kind=%q events=%#v", got, ev)
	}
}

func TestParseCodexAssistantTextMessageIsTurnCompleteTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"hello"}]}}
{"timestamp":"2025-01-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"done"}]}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	ev, _, _, _, _, _ := parse(context.Background(), p)
	if got := ev[len(ev)-1].Kind; got != domain.EventTurnComplete {
		t.Fatalf("last kind=%q events=%#v", got, ev)
	}
	assistants := 0
	for _, e := range ev {
		if e.Kind == domain.EventAssistant && e.Text == "done" {
			assistants++
		}
	}
	if assistants != 1 {
		t.Fatalf("assistant text should remain available for conversation: %#v", ev)
	}
}

func TestCodexConversationExcludesSyntheticTurnComplete(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"hello"}]}}
{"timestamp":"2025-01-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"done"}]}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	pl := &Plugin{}
	c, err := pl.LoadConversation(context.Background(), domain.SessionRef{Source: p})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range c.Nodes {
		for _, e := range n.Events {
			if e.Kind == domain.EventTurnComplete {
				t.Fatalf("synthetic turn_complete should not be in conversation: %#v", c.Nodes)
			}
		}
	}
}

func TestParseCodexWebSearchEndIsToolResultTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"web_search_call"}}
{"timestamp":"2025-01-01T00:00:02Z","type":"event_msg","payload":{"type":"web_search_end"}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	ev, _, _, _, _, _ := parse(context.Background(), p)
	if got := ev[len(ev)-1].Kind; got != domain.EventToolResult {
		t.Fatalf("last kind=%q events=%#v", got, ev)
	}
}

func TestDetectActiveMatchesRolloutUUIDFromOpenFile(t *testing.T) {
	sid := "12345678-1234-1234-1234-123456789abc"
	p := &Plugin{}
	ss, err := p.DetectActive(context.Background(), []domain.Session{{
		PluginID:  "codex",
		AgentType: "codex",
		SessionID: sid,
		SourceRef: domain.SessionRef{Source: "/indexed/location/rollout-2026-06-23T00-00-00-" + sid + ".jsonl"},
		LastKind:  domain.EventUser,
	}}, []domain.Process{{
		OpenFiles: []string{"/different/fd/location/rollout-2026-06-23T00-00-00-" + sid + ".jsonl"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if ss[0].Status != domain.StatusRunning {
		t.Fatalf("codex rollout UUID open file should mark active: %#v", ss[0])
	}
}

func TestDetectActiveMatchesLatestSessionByCodexProcessCWD(t *testing.T) {
	p := &Plugin{o: Options{Executable: "codex"}}
	old := domain.Session{
		PluginID:  "codex",
		AgentType: "codex",
		SessionID: "old",
		CWD:       "/work",
		UpdatedAt: time.Unix(10, 0),
		LastKind:  domain.EventUser,
	}
	newer := domain.Session{
		PluginID:  "codex",
		AgentType: "codex",
		SessionID: "new",
		CWD:       "/work",
		UpdatedAt: time.Unix(20, 0),
		LastKind:  domain.EventTurnComplete,
	}
	other := domain.Session{
		PluginID:  "codex",
		AgentType: "codex",
		SessionID: "other",
		CWD:       "/other",
		UpdatedAt: time.Unix(30, 0),
		LastKind:  domain.EventUser,
	}
	ss, err := p.DetectActive(context.Background(), []domain.Session{old, newer, other}, []domain.Process{{
		Executable: "codex",
		CWD:        "/work",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if ss[0].Status != "" {
		t.Fatalf("older same-cwd session should not be marked active: %#v", ss[0])
	}
	if ss[1].Status != domain.StatusReady {
		t.Fatalf("latest same-cwd session should be active by cwd: %#v", ss[1])
	}
	if ss[2].Status != "" {
		t.Fatalf("different cwd session should not be marked active: %#v", ss[2])
	}
}

func TestDetectActiveMatchesMultipleCodexProcessesByCWD(t *testing.T) {
	p := &Plugin{o: Options{Executable: "codex"}}
	old := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "old", CWD: "/work", UpdatedAt: time.Unix(10, 0), LastKind: domain.EventUser}
	mid := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "mid", CWD: "/work", UpdatedAt: time.Unix(20, 0), LastKind: domain.EventToolCall}
	newer := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "new", CWD: "/work", UpdatedAt: time.Unix(30, 0), LastKind: domain.EventTurnComplete}
	ss, err := p.DetectActive(context.Background(), []domain.Session{old, mid, newer}, []domain.Process{
		{Executable: "codex", CWD: "/work"},
		{Executable: "codex", CWD: "/work"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ss[0].Status != "" {
		t.Fatalf("oldest same-cwd session should remain inactive: %#v", ss[0])
	}
	if ss[1].Status != domain.StatusRunning {
		t.Fatalf("second newest same-cwd session should be active: %#v", ss[1])
	}
	if ss[2].Status != domain.StatusReady {
		t.Fatalf("newest same-cwd session should be active: %#v", ss[2])
	}
}

func TestDetectActiveDeduplicatesCodexWrapperAndRuntimeProcessesByCWD(t *testing.T) {
	p := &Plugin{o: Options{Executable: "codex"}}
	old := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "old", CWD: "/work", UpdatedAt: time.Unix(10, 0), LastKind: domain.EventUser}
	stale := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "stale", CWD: "/work", UpdatedAt: time.Unix(20, 0), LastKind: domain.EventUser}
	activeA := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "active-a", CWD: "/work", UpdatedAt: time.Unix(30, 0), LastKind: domain.EventToolCall}
	activeB := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "active-b", CWD: "/work", UpdatedAt: time.Unix(40, 0), LastKind: domain.EventTurnComplete}
	ss, err := p.DetectActive(context.Background(), []domain.Session{old, stale, activeA, activeB}, []domain.Process{
		{Executable: "node", Args: []string{"node", "/opt/bin/codex"}, CWD: "/work"},
		{Executable: "codex", Args: []string{"/opt/lib/codex/vendor/bin/codex"}, CWD: "/work"},
		{Executable: "node", Args: []string{"node", "/opt/bin/codex"}, CWD: "/work"},
		{Executable: "codex", Args: []string{"/opt/lib/codex/vendor/bin/codex"}, CWD: "/work"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ss[0].Status != "" || ss[1].Status != "" {
		t.Fatalf("wrapper/runtime pairs should count as two active sessions, not four: %#v", ss)
	}
	if ss[2].Status != domain.StatusRunning {
		t.Fatalf("second newest same-cwd session should be active: %#v", ss[2])
	}
	if ss[3].Status != domain.StatusReady {
		t.Fatalf("newest same-cwd session should be active: %#v", ss[3])
	}
}
