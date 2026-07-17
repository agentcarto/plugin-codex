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

// Each event is stamped with the model in force when it was produced. A
// turn_context that changes the model mid-session applies to later events,
// while the session-level model stays the first one seen.
func TestParsePerTurnModel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work","model":"gpt-5-codex"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"a1"}]}}
{"timestamp":"2025-01-01T00:00:02Z","type":"turn_context","payload":{"model":"gpt-5.1-codex"}}
{"timestamp":"2025-01-01T00:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"a2"}]}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	ev, _, _, model, _, _ := parse(context.Background(), p)
	if model != "gpt-5-codex" {
		t.Fatalf("session model = %q, want first model", model)
	}
	got := map[string]string{}
	for _, e := range ev {
		if e.Kind == domain.EventAssistant {
			got[e.Text] = e.Model
		}
	}
	if got["a1"] != "gpt-5-codex" {
		t.Fatalf("a1 model = %q, want gpt-5-codex", got["a1"])
	}
	if got["a2"] != "gpt-5.1-codex" {
		t.Fatalf("a2 model = %q, want gpt-5.1-codex", got["a2"])
	}
}

func TestParseSkipsEmptyReasoning(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"reasoning","encrypted_content":"gAAAA","summary":[]}}
{"timestamp":"2025-01-01T00:00:02Z","type":"response_item","payload":{"type":"reasoning","summary":[{"text":"visible summary"}]}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	ev, _, _, _, _, _ := parse(context.Background(), p)
	var reasoning []string
	for _, e := range ev {
		if e.Kind == domain.EventReasoning {
			reasoning = append(reasoning, e.Text)
		}
	}
	if len(reasoning) != 1 || reasoning[0] != "visible summary" {
		t.Fatalf("expected only the non-empty reasoning, got %#v", reasoning)
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

func TestScanTitleIgnoresMidTurnUserMessages(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn-0"}}
{"timestamp":"2025-01-01T00:00:02Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"timestamp":"2025-01-01T00:00:03Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"# AGENTS.md instructions\n\n<INSTRUCTIONS>rules</INSTRUCTIONS>"},{"text":"<environment_context>cwd=/work</environment_context>"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}
{"timestamp":"2025-01-01T00:00:04Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"initial request"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}
{"timestamp":"2025-01-01T00:00:05Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"working"}],"phase":"commentary","internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}
{"timestamp":"2025-01-01T00:00:06Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"mid-turn clarification"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}
`
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	pl := &Plugin{id: "codex", o: Options{SessionsDir: root}}
	res, err := pl.Scan(context.Background(), plugin.ScanInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 1 {
		t.Fatalf("sessions=%#v", res.Sessions)
	}
	if got := res.Sessions[0].Title; got != "initial request" {
		t.Fatalf("title=%q, want initial request", got)
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

// The new auto-compaction format writes the compacted record in the middle of
// the turn it interrupted (message is empty; the summary is encrypted). The
// boundary must be deferred past the turn's remaining output so the final
// reply stays on its own turn, not on the compaction's.
func TestParseCodexCompactedMidTurnDefersBoundary(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"T1"}}
{"timestamp":"2025-01-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t1"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}
{"timestamp":"2025-01-01T00:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec","arguments":"{}","internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}
{"timestamp":"2025-01-01T00:00:04Z","type":"compacted","payload":{"message":"","replacement_history":[]}}
{"timestamp":"2025-01-01T00:00:05Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"final reply"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}
{"timestamp":"2025-01-01T00:00:06Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"T1"}}
{"timestamp":"2025-01-01T00:00:07Z","type":"event_msg","payload":{"type":"task_started","turn_id":"T2"}}
{"timestamp":"2025-01-01T00:00:08Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t2"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T2"}}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	ev, _, _, _, _, _ := parse(context.Background(), p)
	order := map[string]int{}
	for i, e := range ev {
		if e.RawType == "compact_summary" {
			order["compact"] = i
		}
		if e.Kind == domain.EventAssistant {
			order["reply"] = i
		}
		if e.Kind == domain.EventUser && e.Text == "t2" {
			order["t2"] = i
		}
	}
	if _, ok := order["compact"]; !ok {
		t.Fatalf("compact_summary missing: %#v", ev)
	}
	if !(order["reply"] < order["compact"] && order["compact"] < order["t2"]) {
		t.Fatalf("want reply < compact < t2, got %v", order)
	}
	pl := &Plugin{}
	c, err := pl.LoadConversation(context.Background(), domain.SessionRef{Source: p})
	if err != nil {
		t.Fatal(err)
	}
	turns := convlogic.TurnsOfPath(*c, c.ActivePath())
	turnOf := func(match func(domain.Event) bool) int {
		for ti, turn := range turns {
			for _, id := range turn {
				for _, e := range c.Nodes[id].Events {
					if match(e) {
						return ti
					}
				}
			}
		}
		return -1
	}
	t1 := turnOf(func(e domain.Event) bool { return e.Kind == domain.EventUser && e.Text == "t1" })
	reply := turnOf(func(e domain.Event) bool { return e.Kind == domain.EventAssistant })
	t2 := turnOf(func(e domain.Event) bool { return e.Kind == domain.EventUser && e.Text == "t2" })
	if t1 != reply {
		t.Fatalf("final reply left its turn: t1=%d reply=%d turns=%v", t1, reply, turns)
	}
	if t2 == reply {
		t.Fatalf("final reply leaked into the next turn: %v", turns)
	}
}

// A compaction written between task_started and the turn's first item (compact
// on submit) keeps its position: the boundary stays before the new turn.
func TestParseCodexCompactedAtTurnStartStaysBeforeTurn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"T1"}}
{"timestamp":"2025-01-01T00:00:02Z","type":"compacted","payload":{"message":"","replacement_history":[]}}
{"timestamp":"2025-01-01T00:00:03Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t1"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	ev, _, _, _, _, _ := parse(context.Background(), p)
	compact, user := -1, -1
	for i, e := range ev {
		if e.RawType == "compact_summary" {
			compact = i
		}
		if e.Kind == domain.EventUser && e.Text == "t1" {
			user = i
		}
	}
	if compact < 0 || user < 0 || compact > user {
		t.Fatalf("want compact before user turn, compact=%d user=%d ev=%#v", compact, user, ev)
	}
}

// tool_search_call/tool_search_output must surface as tool events: the call with
// its JSON arguments, the output as the list of found tool names.
func TestParseCodexToolSearch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2025-01-01T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2025-01-01T00:00:01Z","type":"response_item","payload":{"type":"tool_search_call","call_id":"c1","arguments":{"limit":3,"query":"serena"},"metadata":{"turn_id":"T1"}}}
{"timestamp":"2025-01-01T00:00:02Z","type":"response_item","payload":{"type":"tool_search_output","call_id":"c1","status":"completed","tools":[{"name":"mcp__serena","description":"d","tools":[{"name":"initial_instructions"},{"name":"open_dashboard"}]},{"name":"standalone"}]}}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	ev, _, _, _, _, _ := parse(context.Background(), p)
	if len(ev) != 2 {
		t.Fatalf("want 2 events, got %#v", ev)
	}
	call, out := ev[0], ev[1]
	if call.Kind != domain.EventToolCall || call.ToolName != "tool_search" || call.TurnID != "T1" {
		t.Fatalf("call=%#v", call)
	}
	if !strings.Contains(call.Text, `"query":"serena"`) {
		t.Fatalf("call args lost: %q", call.Text)
	}
	if out.Kind != domain.EventToolResult {
		t.Fatalf("out=%#v", out)
	}
	if out.Text != "mcp__serena: initial_instructions, open_dashboard\nstandalone" {
		t.Fatalf("out text=%q", out.Text)
	}
}

func TestParseCodexSubagentFinalAnswerAsTask(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-x-id.jsonl")
	data := `{"timestamp":"2026-07-17T00:00:00Z","type":"session_meta","payload":{"id":"id","cwd":"/work"}}
{"timestamp":"2026-07-17T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"review"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}
{"timestamp":"2026-07-17T00:00:02Z","type":"response_item","payload":{"type":"agent_message","author":"/root/security_review","recipient":"/root","content":[{"type":"input_text","text":"Message Type: MESSAGE\nTask name: /root\nSender: /root/security_review\nPayload:\nprogress"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}
{"timestamp":"2026-07-17T00:00:03Z","type":"response_item","payload":{"type":"agent_message","author":"/root/security_review","recipient":"/root","content":[{"type":"input_text","text":"Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/security_review\nPayload:\nOne issue found.\n\nDetails here."}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}
`
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	events, _, _, _, _, _ := parse(context.Background(), p)
	var tasks []domain.Event
	for _, event := range events {
		if event.Kind == domain.EventTask {
			tasks = append(tasks, event)
		}
	}
	if len(tasks) != 1 {
		t.Fatalf("want one completed task event, got %#v", events)
	}
	task := tasks[0]
	if task.ToolArg != "security_review [completed]" || task.ToolDetail != "One issue found.\n\nDetails here." || task.TurnID != "T1" {
		t.Fatalf("task=%#v", task)
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

func TestScanCodexSkipsInternalSubagent(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "07", "17")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}

	parentPath := filepath.Join(day, "rollout-2026-07-17T00-00-00-parent.jsonl")
	parentData := `{"timestamp":"2026-07-17T00:00:00Z","type":"session_meta","payload":{"id":"parent","cwd":"/parent","model":"parent-model"}}
{"timestamp":"2026-07-17T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"parent prompt"}]}}
`
	if err := os.WriteFile(parentPath, []byte(parentData), 0600); err != nil {
		t.Fatal(err)
	}

	childPath := filepath.Join(day, "rollout-2026-07-17T00-01-00-child.jsonl")
	childData := `{"timestamp":"2026-07-17T00:01:00Z","type":"session_meta","payload":{"id":"child","cwd":"/child","model":"child-model","forked_from_id":"parent","thread_source":"subagent"}}
{"timestamp":"2026-07-17T00:00:00Z","type":"session_meta","payload":{"id":"parent","cwd":"/parent","model":"parent-model"}}
{"timestamp":"2026-07-17T00:01:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"child prompt"}]}}
`
	if err := os.WriteFile(childPath, []byte(childData), 0600); err != nil {
		t.Fatal(err)
	}

	p := &Plugin{id: "codex", o: Options{SessionsDir: root}}
	res, err := p.Scan(context.Background(), plugin.ScanInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 1 {
		t.Fatalf("internal subagent should be excluded, got %#v", res.Sessions)
	}
	if res.Sessions[0].SessionID != "parent" {
		t.Fatalf("parent session missing: %#v", res.Sessions)
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

func TestDetectActiveIgnoresAppServerProcessesMatchedOnlyByCWD(t *testing.T) {
	p := &Plugin{o: Options{Executable: "codex"}}
	sessions := []domain.Session{{
		PluginID:  "codex",
		AgentType: "codex",
		SessionID: "completed",
		CWD:       "/work",
		UpdatedAt: time.Unix(20, 0),
		LastKind:  domain.EventTurnComplete,
	}}
	processes := []domain.Process{
		{PID: 1, Executable: "phpstorm"},
		{PID: 2, PPID: 1, Executable: "node", Args: []string{"node", "/opt/node_modules/@openai/codex/bin/codex.js", "app-server"}, CWD: "/work"},
		{PID: 3, PPID: 2, Executable: "codex", Args: []string{"/opt/vendor/bin/codex", "app-server"}, CWD: "/work"},
	}

	ss, err := p.DetectActive(context.Background(), sessions, processes)
	if err != nil {
		t.Fatal(err)
	}
	if ss[0].Status != "" {
		t.Fatalf("processes without direct session evidence should not mark a same-cwd session active: %#v", ss[0])
	}
}

func TestDetectActiveFallsBackToCWDForIDEIntegratedTerminal(t *testing.T) {
	p := &Plugin{o: Options{Executable: "codex"}}
	sessions := []domain.Session{{
		PluginID:  "codex",
		AgentType: "codex",
		SessionID: "ide-terminal-session",
		CWD:       "/work",
		UpdatedAt: time.Unix(20, 0),
		LastKind:  domain.EventTurnComplete,
	}}
	processes := []domain.Process{
		{PID: 1, Executable: "phpstorm"},
		// JetBrains can launch a terminal command directly, without retaining the
		// shell as an ancestor of the Codex wrapper/runtime pair.
		{PID: 2, PPID: 1, Executable: "node", Args: []string{"node", "/opt/bin/codex"}, CWD: "/work"},
		{PID: 3, PPID: 2, Executable: "codex", Args: []string{"/opt/vendor/bin/codex"}, CWD: "/work"},
	}

	ss, err := p.DetectActive(context.Background(), sessions, processes)
	if err != nil {
		t.Fatal(err)
	}
	if ss[0].Status != domain.StatusReady {
		t.Fatalf("IDE integrated terminal should retain the cwd fallback: %#v", ss[0])
	}
}

func TestDetectActiveFallsBackToCWDForTerminalProcess(t *testing.T) {
	p := &Plugin{o: Options{Executable: "codex"}}
	sessions := []domain.Session{{
		PluginID:  "codex",
		AgentType: "codex",
		SessionID: "terminal-session",
		CWD:       "/work",
		UpdatedAt: time.Unix(20, 0),
		LastKind:  domain.EventTurnComplete,
	}}
	processes := []domain.Process{
		{PID: 1, Executable: "zsh"},
		{PID: 2, PPID: 1, Executable: "node", Args: []string{"node", "/opt/bin/codex"}, CWD: "/work"},
		{PID: 3, PPID: 2, Executable: "codex", Args: []string{"/opt/vendor/bin/codex"}, CWD: "/work"},
	}

	ss, err := p.DetectActive(context.Background(), sessions, processes)
	if err != nil {
		t.Fatal(err)
	}
	if ss[0].Status != domain.StatusReady {
		t.Fatalf("terminal-launched process should retain the cwd fallback: %#v", ss[0])
	}
}

func TestDetectActiveMatchesAppServerWithOpenRollout(t *testing.T) {
	sid := "12345678-1234-1234-1234-123456789abc"
	p := &Plugin{o: Options{Executable: "codex"}}
	sessions := []domain.Session{{
		PluginID:  "codex",
		AgentType: "codex",
		SessionID: sid,
		CWD:       "/work",
		SourceRef: domain.SessionRef{Source: "/sessions/rollout-" + sid + ".jsonl"},
		LastKind:  domain.EventUser,
	}}
	processes := []domain.Process{{
		Executable: "codex",
		Args:       []string{"/opt/vendor/bin/codex", "app-server"},
		OpenFiles:  []string{"/sessions/rollout-" + sid + ".jsonl"},
		CWD:        "/work",
	}}

	ss, err := p.DetectActive(context.Background(), sessions, processes)
	if err != nil {
		t.Fatal(err)
	}
	if ss[0].Status != domain.StatusRunning {
		t.Fatalf("app-server with direct rollout evidence should remain active: %#v", ss[0])
	}
}

// codexTurnStarts must count exactly what the display counts as turns: queued
// input (after an abort or with an already-seen turn_id) is not a turn,
// compaction summaries are, and thread_rolled_back drops popped turns.
func TestCodexTurnStartsMatchesDisplayRules(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"session_meta","payload":{"id":"id","cwd":"/w"}}`),
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t1"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}`),
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"queued same turn"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}`),
		[]byte(`{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"T1"}}`),
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"queued after abort"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}`),
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t2"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T2"}}}`),
		[]byte(`{"type":"compacted","payload":{"message":"Your conversation was summarized"}}`),
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t3"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T3"}}}`),
	}
	got := codexTurnStarts(lines)
	// t1, t2, compaction, t3 — the two queued messages don't start turns. The
	// compaction follows response items with no task_started in between, so its
	// boundary is deferred to the next user turn (line 7), like the parser does.
	want := []int{1, 5, 7, 7}
	if len(got) != len(want) {
		t.Fatalf("starts=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("starts=%v want %v", got, want)
		}
	}
}

func TestCodexTurnStartsRollbackDropsPoppedTurns(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t1"}]}}`),
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t2"}]}}`),
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t3"}]}}`),
		[]byte(`{"type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":2}}`),
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t2'"}]}}`),
	}
	got := codexTurnStarts(lines)
	want := []int{0, 4} // t1 and t2' survive; t2, t3 were rolled back
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("starts=%v want %v", got, want)
	}
}

// A compaction record in the middle of a turn's items starts its displayed turn
// at the next task_started, so cutting there keeps the interrupted turn whole
// (including the final reply written after the compacted line).
func TestCodexTurnStartsDefersMidTurnCompaction(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"session_meta","payload":{"id":"abc","cwd":"/w"}}`),
		[]byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"T1"}}`),
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t1"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}`),
		[]byte(`{"type":"compacted","payload":{"message":"","replacement_history":[]}}`),
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"final reply"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}`),
		[]byte(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"T1"}}`),
		[]byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"T2"}}`),
		[]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t2"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T2"}}}`),
	}
	got := codexTurnStarts(lines)
	want := []int{2, 6, 7} // t1; deferred compaction at the next task_started; t2
	if len(got) != len(want) {
		t.Fatalf("starts=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("starts=%v want %v", got, want)
		}
	}
}

// Forking right after a turn interrupted by auto-compaction keeps that turn's
// trailing output, which codex wrote after the compacted record.
func TestPlanForkKeepsReplyWrittenAfterMidTurnCompaction(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "rollout-2025-01-01T00-00-00-abc.jsonl")
	data := `{"type":"session_meta","payload":{"id":"abc","cwd":"/w"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"T1"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t1"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}
{"type":"compacted","payload":{"message":"","replacement_history":[]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"final reply"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"T1"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"T2"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t2"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T2"}}}
`
	if e := os.WriteFile(f, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	p := &Plugin{id: "codex", o: Options{SessionsDir: dir, Executable: "codex"}}
	s := domain.Session{PluginID: "codex", SessionID: "abc", SourceRef: domain.SessionRef{Source: f}}
	plan, _, e := p.PlanFork(context.Background(), s, domain.ForkTarget{KeepTurns: 1})
	if e != nil {
		t.Fatal(e)
	}
	out := string(plan.Writes[0].Data)
	if !strings.Contains(out, "final reply") {
		t.Fatalf("fork lost the reply written after the compacted record:\n%s", out)
	}
	if strings.Contains(out, `"t2"`) {
		t.Fatalf("fork kept content past the cut:\n%s", out)
	}
}

// A fork of a session containing queued input must cut at the displayed-turn
// boundary: KeepTurns counts displayed turns, not raw user messages.
func TestPlanForkCutsAtDisplayedTurnWithQueuedInput(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "rollout-2025-01-01T00-00-00-abc.jsonl")
	data := `{"type":"session_meta","payload":{"id":"abc","cwd":"/w"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t1"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"queued"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T1"}}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t2"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T2"}}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t3"}],"internal_chat_message_metadata_passthrough":{"turn_id":"T3"}}}
`
	if e := os.WriteFile(f, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	p := &Plugin{id: "codex", o: Options{SessionsDir: dir, Executable: "codex"}}
	s := domain.Session{PluginID: "codex", SessionID: "abc", SourceRef: domain.SessionRef{Source: f}}
	// Keep 2 displayed turns: t1 (with its queued message) and t2; t3 is dropped.
	plan, _, e := p.PlanFork(context.Background(), s, domain.ForkTarget{KeepTurns: 2})
	if e != nil {
		t.Fatal(e)
	}
	out := string(plan.Writes[0].Data)
	if !strings.Contains(out, "queued") || !strings.Contains(out, `"t2"`) {
		t.Fatalf("fork lost kept content:\n%s", out)
	}
	if strings.Contains(out, `"t3"`) {
		t.Fatalf("fork kept content past the cut:\n%s", out)
	}
	// KeepTurns beyond the displayed total is rejected.
	if _, _, e := p.PlanFork(context.Background(), s, domain.ForkTarget{KeepTurns: 4}); e == nil {
		t.Fatal("want out-of-range error: raw user messages must not inflate the total")
	}
}

// Untouched lines of the fork copy survive byte-for-byte (numbers, escaping).
func TestPlanForkPreservesLineFidelity(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "rollout-2025-01-01T00-00-00-abc.jsonl")
	raw := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"t1"}]},"big":9007199254740993,"s":"<x>"}`
	data := `{"type":"session_meta","payload":{"id":"abc","cwd":"/w"}}` + "\n" + raw + "\n"
	if e := os.WriteFile(f, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	p := &Plugin{id: "codex", o: Options{SessionsDir: dir, Executable: "codex"}}
	s := domain.Session{PluginID: "codex", SessionID: "abc", SourceRef: domain.SessionRef{Source: f}}
	plan, _, e := p.PlanFork(context.Background(), s, domain.ForkTarget{KeepTurns: 1})
	if e != nil {
		t.Fatal(e)
	}
	out := string(plan.Writes[0].Data)
	if !strings.Contains(out, raw) {
		t.Fatalf("non-meta line was rewritten:\n%s", out)
	}
	if !strings.Contains(out, `"forked_from_id":"abc"`) {
		t.Fatalf("session_meta not tagged:\n%s", out)
	}
}
