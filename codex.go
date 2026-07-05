package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agentcarto/core/common"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"github.com/agentcarto/core/scan"
	"gopkg.in/yaml.v3"
)

type Options struct {
	SessionsDir string `yaml:"sessions_dir"`
	Executable  string `yaml:"executable"`
}

type Factory struct{}

func (Factory) Descriptor() plugin.Descriptor {
	// ParserVersion=6: user events now carry the normalized Prompt field
	// (agent-specific pseudo-prompt vocabulary moved out of core).
	// ParserVersion=7: tool calls carry ToolArg and apply_patch/file_change
	// events carry Changes (agent-specific rendering moved out of the host).
	return plugin.Descriptor{Type: "codex", DisplayName: "Codex", ParserVersion: "7", Capabilities: domain.Capabilities{Scan: true, Conversation: true, Active: true, Resume: true, Rewind: true, Relocate: true}}
}

func (Factory) New(id string, n *yaml.Node) (any, error) {
	o := Options{SessionsDir: "~/.codex/sessions", Executable: "codex"}
	if e := common.DecodeOptions(n, &o); e != nil {
		return nil, e
	}
	o.SessionsDir = common.ExpandHome(o.SessionsDir)
	return &Plugin{id, o}, nil
}

type Plugin struct {
	id string
	o  Options
}

func (p *Plugin) Executable() string { return p.o.Executable }

// codexParse accumulates the state needed while walking a rollout's JSONL lines.
type codexParse struct {
	events []domain.Event
	cwd    string
	id     string
	model  string
	// curModel is the model in force for records seen so far. session_meta and
	// each turn_context update it, and every emitted event is stamped with it so
	// the host can show per-turn models. model (above) keeps the first value as
	// the session-level fallback.
	curModel string
	parent   string
	start    time.Time
	// seenUserTurn tracks turn IDs whose first user message has already been
	// emitted, so later messages on the same turn are marked queued.
	seenUserTurn map[string]bool
	// pendingAbortTurnID holds the turn that was just aborted, so the next user
	// message is attached to it as queued input.
	pendingAbortTurnID string
	// turnHasItems reports whether the turn in flight has produced response items.
	// Codex writes an auto-compaction record either before a turn's items (compact
	// on submit) or in their middle (compact while streaming); only the latter must
	// be deferred so the turn's remaining output stays on its own side of the boundary.
	turnHasItems bool
	// pendingCompacts holds mid-turn compaction events until the turn's items are
	// done (the next task_started, or end of file).
	pendingCompacts []domain.Event
}

func parse(ctx context.Context, path string) ([]domain.Event, string, string, string, string, time.Time) {
	s := &codexParse{seenUserTurn: map[string]bool{}}
	_ = common.JSONLines(ctx, path, func(_ int, o map[string]any) error {
		s.consume(o)
		return nil
	})
	s.flushCompacts()
	annotateTools(s.events)
	if s.id == "" {
		s.id = common.IDFromPath(path)
	}
	return s.events, s.cwd, s.id, s.model, s.parent, s.start
}

// flushCompacts emits the compaction events deferred from the middle of a turn.
func (s *codexParse) flushCompacts() {
	s.events = append(s.events, s.pendingCompacts...)
	s.pendingCompacts = nil
}

// consume processes a single decoded JSONL record, updating session metadata
// and appending any conversation event it produces.
func (s *codexParse) consume(o map[string]any) {
	ts := common.Time(common.String(o["timestamp"]))
	if s.start.IsZero() && !ts.IsZero() {
		s.start = ts
	}
	typ := common.String(o["type"])
	p := common.Map(o["payload"])
	switch typ {
	case "session_meta":
		s.id = common.String(p["id"])
		s.cwd = common.String(p["cwd"])
		s.model = common.String(p["model"])
		s.curModel = s.model
		s.parent = common.String(p["forked_from_id"])
		return
	case "turn_context":
		if s.cwd == "" {
			s.cwd = common.String(p["cwd"])
		}
		if m := common.String(p["model"]); m != "" {
			if s.model == "" {
				s.model = m
			}
			s.curModel = m
		}
		return
	}

	turnID := codexTurnID(p)
	var e *domain.Event
	switch typ {
	case "compacted":
		ce := domain.Event{Kind: domain.EventUser, Text: strings.TrimSpace(common.String(p["message"])), Timestamp: ts, RawType: domain.RawCompactSummary, TurnID: turnID}
		if s.turnHasItems {
			s.pendingCompacts = append(s.pendingCompacts, ce)
			return
		}
		e = &ce
	case "response_item":
		s.turnHasItems = true
		e = s.responseItem(p, ts, turnID)
	case "event_msg":
		if common.String(p["type"]) == "task_started" {
			s.flushCompacts()
			s.turnHasItems = false
		}
		e = s.eventMsg(p, ts, turnID)
	}
	if e != nil {
		if e.Model == "" {
			e.Model = s.curModel
		}
		s.events = append(s.events, *e)
	}
}

// responseItem maps a "response_item" payload to its conversation event.
func (s *codexParse) responseItem(p map[string]any, ts time.Time, turnID string) *domain.Event {
	pt := common.String(p["type"])
	switch pt {
	case "message":
		return s.message(p, ts, turnID)
	case "reasoning":
		text := common.Text(p["content"])
		if text == "" {
			text = common.Text(p["summary"])
		}
		// Reasoning is stored encrypted (encrypted_content) with an empty plaintext
		// summary unless model_reasoning_summary is enabled; skip rather than emit a
		// blank reasoning event.
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return &domain.Event{Kind: domain.EventReasoning, Text: text, Timestamp: ts, RawType: pt, TurnID: turnID}
	case "function_call", "local_shell_call", "custom_tool_call", "web_search_call":
		text := common.String(p["arguments"])
		if text == "" {
			text = common.String(p["input"])
		}
		return &domain.Event{Kind: domain.EventToolCall, Text: text, Timestamp: ts, ToolName: common.String(p["name"]), RawType: pt, TurnID: turnID}
	case "function_call_output", "local_shell_call_output", "custom_tool_call_output", "web_search_call_output":
		return &domain.Event{Kind: domain.EventToolResult, Text: codexOutputText(p["output"]), Timestamp: ts, RawType: pt, TurnID: turnID}
	case "tool_search_call":
		// Arguments are a JSON object here, unlike function_call's pre-encoded
		// string (json.Marshal sorts map keys, so the text is deterministic).
		args, _ := json.Marshal(p["arguments"])
		return &domain.Event{Kind: domain.EventToolCall, Text: string(args), Timestamp: ts, ToolName: "tool_search", RawType: pt, TurnID: turnID}
	case "tool_search_output":
		return &domain.Event{Kind: domain.EventToolResult, Text: toolSearchResultText(p["tools"]), Timestamp: ts, RawType: pt, TurnID: turnID}
	}
	return nil
}

// toolSearchResultText renders a tool_search_output "tools" list as one line per
// entry: the server/tool name followed by its nested tool names. Descriptions and
// parameter schemas are omitted; the names are what identify the search result.
func toolSearchResultText(v any) string {
	var lines []string
	for _, t := range common.Slice(v) {
		m := common.Map(t)
		name := common.String(m["name"])
		if name == "" {
			continue
		}
		var subs []string
		for _, st := range common.Slice(m["tools"]) {
			if n := common.String(common.Map(st)["name"]); n != "" {
				subs = append(subs, n)
			}
		}
		if len(subs) > 0 {
			name += ": " + strings.Join(subs, ", ")
		}
		lines = append(lines, name)
	}
	return strings.Join(lines, "\n")
}

// message handles a chat message payload. Assistant messages emit their text
// (when non-empty) plus a synthetic turn-complete marker; everything else is
// classified into the appropriate event kind.
func (s *codexParse) message(p map[string]any, ts time.Time, turnID string) *domain.Event {
	role := common.String(p["role"])
	text := common.Text(p["content"])
	if role == "assistant" {
		if strings.TrimSpace(text) != "" {
			s.events = append(s.events, domain.Event{Kind: domain.EventAssistant, Text: text, Timestamp: ts, RawType: "message", TurnID: turnID, Model: s.curModel})
		}
		return &domain.Event{Kind: domain.EventTurnComplete, Timestamp: ts, RawType: "turn_complete", TurnID: turnID}
	}
	kind := messageKind(role, text)
	if kind == domain.EventUser {
		kind, turnID = s.classifyUserMessage(turnID)
		if kind == domain.EventUser {
			// A new user turn ends any turn a compaction interrupted; place the
			// deferred boundary before the turn (rollouts without task_started
			// records have no other flush point).
			s.flushCompacts()
		}
	}
	ev := domain.Event{Kind: kind, Text: text, Timestamp: ts, RawType: "message", TurnID: turnID}
	if kind == domain.EventUser {
		ev.Prompt = promptText(text)
	}
	return &ev
}

// messageKind decides the base event kind for a non-assistant message. Roles
// other than "user" (e.g. "developer") are system-injected rather than real
// user input, and environment preambles are likewise treated as system events
// to avoid mis-rendering and polluting titles/headings.
func messageKind(role, text string) domain.EventKind {
	if codexIsPreamble(text) || strings.Contains(text, "# AGENTS.md instructions") {
		return domain.EventSystem
	}
	if role == "user" {
		return domain.EventUser
	}
	return domain.EventSystem
}

// classifyUserMessage decides whether a user message starts a new turn or is
// queued input on an existing/aborted turn, and returns the turn it belongs to.
func (s *codexParse) classifyUserMessage(turnID string) (domain.EventKind, string) {
	switch {
	case s.pendingAbortTurnID != "":
		aborted := s.pendingAbortTurnID
		s.pendingAbortTurnID = ""
		return domain.EventQueued, aborted
	case turnID != "" && s.seenUserTurn[turnID]:
		return domain.EventQueued, turnID
	default:
		s.seenUserTurn[turnID] = true
		return domain.EventUser, turnID
	}
}

// eventMsg handles the "event_msg" stream. Most of these duplicate response
// items and are dropped; only the few below carry information we keep.
func (s *codexParse) eventMsg(p map[string]any, ts time.Time, turnID string) *domain.Event {
	switch common.String(p["type"]) {
	case "turn_aborted":
		s.pendingAbortTurnID = common.String(p["turn_id"])
	case "thread_rolled_back":
		return &domain.Event{Kind: domain.EventMeta, Text: fmt.Sprint(p["num_turns"]), Timestamp: ts, RawType: "thread_rolled_back", TurnID: turnID}
	case "web_search_end":
		return &domain.Event{Kind: domain.EventToolResult, Timestamp: ts, RawType: "web_search_end", TurnID: turnID}
	case "patch_apply_end":
		if ok, _ := p["success"].(bool); ok {
			if changes := common.Map(p["changes"]); len(changes) > 0 {
				return &domain.Event{Kind: domain.EventFileChange, Text: patchDocument(changes), Timestamp: ts, RawType: "patch_apply_end", TurnID: turnID}
			}
		}
	}
	return nil
}

func codexTurnID(p map[string]any) string {
	if id := common.String(common.Map(p["internal_chat_message_metadata_passthrough"])["turn_id"]); id != "" {
		return id
	}
	// tool_search_call and similar newer items carry the turn in "metadata" instead.
	return common.String(common.Map(p["metadata"])["turn_id"])
}

// patchDocument renders a patch_apply_end "changes" map as an apply_patch document
// (the same *** Begin/End Patch format Codex uses for the request) so the host can
// show the real per-file diff rather than only aggregate counts. Update files embed
// their unified_diff verbatim; add/delete files list their content as +/- lines.
// The host derives added/removed counts from these +/- lines (codexPatchStats).
func patchDocument(changes map[string]any) string {
	names := make([]string, 0, len(changes))
	for name := range changes {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("*** Begin Patch")
	for _, name := range names {
		m := common.Map(changes[name])
		switch common.String(m["type"]) {
		case "add":
			b.WriteString("\n*** Add File: " + name)
			for _, ln := range patchBodyLines(common.String(m["content"])) {
				b.WriteString("\n+" + ln)
			}
		case "delete":
			b.WriteString("\n*** Delete File: " + name)
			for _, ln := range patchBodyLines(common.String(m["content"])) {
				b.WriteString("\n-" + ln)
			}
		default: // update / rename
			b.WriteString("\n*** Update File: " + name)
			if ud := strings.TrimRight(common.String(m["unified_diff"]), "\n"); ud != "" {
				b.WriteString("\n" + ud)
			}
		}
	}
	b.WriteString("\n*** End Patch")
	return b.String()
}

// patchBodyLines splits file content into lines, dropping a single trailing newline
// and returning nil for empty content (so an empty file contributes no body lines).
func patchBodyLines(content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(content, "\n"), "\n")
}

// codexOutputText flattens tool output into plain text, unwrapping the JSON
// "chunk" envelopes Codex uses and stripping their human-readable metadata lines.
func codexOutputText(v any) string {
	var out []string
	for _, line := range strings.Split(common.Text(v), "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			out = append(out, line)
			continue
		}
		if chunk, ok := decodeChunk(s); ok {
			out = append(out, strings.Split(strings.TrimSuffix(chunk, "\n"), "\n")...)
			continue
		}
		if isChunkMetadataLine(s) {
			continue
		}
		out = append(out, line)
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// decodeChunk returns the inner output of a JSON chunk envelope, if the line is one.
func decodeChunk(s string) (string, bool) {
	var chunk struct {
		ChunkID            string `json:"chunk_id"`
		WallTimeSeconds    any    `json:"wall_time_seconds"`
		ExitCode           any    `json:"exit_code"`
		OriginalTokenCount any    `json:"original_token_count"`
		Output             string `json:"output"`
	}
	if json.Unmarshal([]byte(s), &chunk) == nil && chunk.ChunkID != "" {
		return chunk.Output, true
	}
	return "", false
}

// isChunkMetadataLine reports whether a line is one of Codex's human-readable
// chunk metadata headers that should not appear in the rendered output.
func isChunkMetadataLine(s string) bool {
	return strings.HasPrefix(s, "Chunk ID:") ||
		strings.HasPrefix(s, "Wall time:") ||
		strings.HasPrefix(s, "Wall time ") ||
		strings.HasPrefix(s, "Process exited") ||
		strings.HasPrefix(s, "Original token count:") ||
		s == "Output:" ||
		strings.HasPrefix(s, "Script completed")
}

func (p *Plugin) Scan(ctx context.Context, in plugin.ScanInput) (plugin.ScanOutput, error) {
	cache := scan.New(in.Warm, in.Dead, Factory{}.Descriptor().ParserVersion)
	fs, e := common.WalkFiles(p.o.SessionsDir, isRolloutFile)
	if e != nil {
		return plugin.ScanOutput{}, e
	}
	var out []domain.Session
	for _, f := range fs {
		if s, ok := cache.Reuse(f); ok {
			out = append(out, s)
			continue
		}
		if cache.Skip(f) {
			continue
		}
		ev, cwd, id, m, parent, st := parse(ctx, f)
		if len(ev) == 0 {
			cache.Dead(f)
			continue
		}
		if cwd == "" {
			cwd = "(unknown)"
		}
		if st.IsZero() {
			st = common.FileTime(f)
		}
		s := domain.Session{PluginID: p.id, AgentType: "codex", SessionID: id, CWD: cwd, StartedAt: st, UpdatedAt: common.FileTime(f), Title: common.Title(ev, "(no title)"), Model: m, ParentSessionID: parent, SourceRef: domain.SessionRef{Source: f}, LastKind: common.LastMeaningful(ev)}
		cache.Stamp(&s)
		out = append(out, s)
	}
	return plugin.ScanOutput{Sessions: out, Dead: cache.DeadOut()}, nil
}

// isRolloutFile reports whether a path is a Codex rollout log.
func isRolloutFile(path string) bool {
	return filepath.Ext(path) == ".jsonl" && strings.HasPrefix(filepath.Base(path), "rollout-")
}

func (p *Plugin) LoadConversation(ctx context.Context, r domain.SessionRef) (*domain.Conversation, error) {
	ev, _, _, _, _, _ := parse(ctx, r.Source)
	ev = withoutSyntheticStatus(ev)
	c := rollbackConversation(ev, "thread_rolled_back")
	return &c, nil
}

// withoutSyntheticStatus drops the synthetic turn-complete markers that parse
// emits for active-status detection but that should not appear in conversations.
func withoutSyntheticStatus(events []domain.Event) []domain.Event {
	out := events[:0]
	for _, e := range events {
		if e.Kind == domain.EventTurnComplete && e.RawType == "turn_complete" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func rollbackConversation(ev []domain.Event, marker string) domain.Conversation {
	var nodes []domain.ConvNode
	parent := ""
	users := []string{}
	lastUserTurnID := ""
	for i, e := range ev {
		if e.RawType == marker {
			parent = rollbackTo(e.Text, nodes, &users, parent)
			continue
		}
		id := fmt.Sprintf("event-%08d", i)
		nodes = append(nodes, domain.ConvNode{ID: id, Parent: parent, Timestamp: e.Timestamp, Events: []domain.Event{e}})
		parent = id
		// Only real user turns count toward rollback; skip preambles and compact summaries.
		if isRealUserTurn(e) {
			if e.TurnID == "" || e.TurnID != lastUserTurnID {
				users = append(users, id)
			}
			if e.TurnID != "" {
				lastUserTurnID = e.TurnID
			}
		}
	}
	return domain.NewConversation(nodes)
}

// rollbackTo rewinds the conversation by the number of user turns encoded in
// markerText, trimming the user-turn stack and returning the new parent node ID.
func rollbackTo(markerText string, nodes []domain.ConvNode, users *[]string, parent string) string {
	n := 0
	fmt.Sscan(markerText, &n)
	if n <= 0 || n > len(*users) {
		return parent
	}
	target := (*users)[len(*users)-n]
	*users = (*users)[:len(*users)-n]
	for _, x := range nodes {
		if x.ID == target {
			return x.Parent
		}
	}
	return ""
}

// isRealUserTurn reports whether an event represents a genuine user turn (not a
// preamble or compact summary) for the purposes of rollback counting.
func isRealUserTurn(e domain.Event) bool {
	return e.Kind == domain.EventUser && e.RawType != domain.RawCompactSummary && !codexIsPreamble(e.Text)
}

// codexPreambles are the wrappers Codex injects into user messages; messages
// starting with one of these are not counted as real turns.
var codexPreambles = []string{"<environment_context>", "<user_instructions>", "<turn_aborted>"}

func codexIsPreamble(text string) bool {
	t := strings.TrimLeft(text, " \t\r\n")
	for _, p := range codexPreambles {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// promptText returns the cleaned, whitespace-folded genuine prompt in text,
// or "" when the message is a Codex-injected preamble, an AGENTS.md dump, or
// a short single-line slash command rather than real user input.
func promptText(text string) string {
	t := strings.TrimSpace(text)
	if t == "" || codexIsPreamble(t) {
		return ""
	}
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "# agents.md instructions") || strings.HasPrefix(low, "agents.md instructions") {
		return ""
	}
	if common.IsBareSlashCommand(t) {
		return ""
	}
	return strings.Join(strings.Fields(t), " ")
}

func (p *Plugin) ResumeCommand(s domain.Session) (domain.Command, error) {
	if s.Status != "" {
		return domain.Command{}, fmt.Errorf("session is active")
	}
	if _, e := os.Stat(s.CWD); e != nil {
		return domain.Command{}, e
	}
	return domain.Command{Executable: p.o.Executable, Args: []string{"resume", s.SessionID}, WorkingDirectory: s.CWD}, nil
}

func (p *Plugin) DetectActive(ctx context.Context, ss []domain.Session, ps []domain.Process) ([]domain.Session, error) {
	matched := map[string]bool{}
	cwdCounts := p.countCodexProcessesByCWD(ss, ps, matched)
	for cwd, count := range cwdCounts {
		markLatestSessionsByCWD(ss, matched, cwd, count)
	}
	for i := range ss {
		if matched[ss[i].SessionID] {
			ss[i].Status = common.ActiveStatus(ss[i].LastKind, true)
			ss[i].PermissionWait = permissionWait(ctx, ss[i].SourceRef.Source)
		}
	}
	return ss, nil
}

// countCodexProcessesByCWD marks sessions that match a process exactly and, for
// the remaining Codex processes, counts how many run in each working directory.
// A direct runtime and its wrapper share a CWD, so direct counts replace wrapper
// counts for the same directory to avoid double-counting a single session.
func (p *Plugin) countCodexProcessesByCWD(ss []domain.Session, ps []domain.Process, matched map[string]bool) map[string]int {
	directCWDCounts := map[string]int{}
	wrapperCWDCounts := map[string]int{}
	for _, pr := range ps {
		exact := false
		for i := range ss {
			if codexProcessMatches(ss[i], []domain.Process{pr}) {
				matched[ss[i].SessionID] = true
				exact = true
			}
		}
		if !p.isCodexProcess(pr) {
			continue
		}
		if pr.CWD != "" && !exact {
			if p.isCodexDirectProcess(pr) {
				directCWDCounts[pr.CWD]++
			} else {
				wrapperCWDCounts[pr.CWD]++
			}
		}
	}
	cwdCounts := map[string]int{}
	for cwd, count := range wrapperCWDCounts {
		cwdCounts[cwd] = count
	}
	for cwd, count := range directCWDCounts {
		cwdCounts[cwd] = count
	}
	return cwdCounts
}

// markLatestSessionsByCWD marks the `count` most recently updated unmatched
// sessions in the given working directory as active.
func markLatestSessionsByCWD(ss []domain.Session, matched map[string]bool, cwd string, count int) {
	for n := 0; n < count; n++ {
		best := -1
		for i := range ss {
			if matched[ss[i].SessionID] || ss[i].CWD != cwd {
				continue
			}
			if best < 0 || ss[i].UpdatedAt.After(ss[best].UpdatedAt) {
				best = i
			}
		}
		if best < 0 {
			return
		}
		matched[ss[best].SessionID] = true
	}
}

// executableName returns the configured executable's base name, defaulting to "codex".
func (p *Plugin) executableName() string {
	name := filepath.Base(p.o.Executable)
	if name == "" || name == "." {
		return "codex"
	}
	return name
}

// matchesExecName reports whether a path token resolves to the given executable
// name, with or without its file extension.
func matchesExecName(token, name string) bool {
	base := filepath.Base(token)
	return base == name || strings.TrimSuffix(base, filepath.Ext(base)) == name
}

// isCodexProcess reports whether a process is Codex, matching either its
// executable or any of its arguments (covers wrapper launchers like node).
func (p *Plugin) isCodexProcess(pr domain.Process) bool {
	name := p.executableName()
	for _, tok := range append([]string{pr.Executable}, pr.Args...) {
		if matchesExecName(tok, name) {
			return true
		}
	}
	return false
}

// isCodexDirectProcess reports whether the process executable itself is Codex
// (as opposed to a wrapper that merely passes Codex as an argument).
func (p *Plugin) isCodexDirectProcess(pr domain.Process) bool {
	return matchesExecName(pr.Executable, p.executableName())
}

func codexProcessMatches(s domain.Session, ps []domain.Process) bool {
	if common.ProcessMatches(s, ps) {
		return true
	}
	if s.SessionID == "" {
		return false
	}
	for _, p := range ps {
		for _, f := range p.OpenFiles {
			base := filepath.Base(f)
			if strings.HasPrefix(base, "rollout-") && strings.HasSuffix(base, ".jsonl") && strings.Contains(base, s.SessionID) {
				return true
			}
		}
	}
	return false
}

// codexPermEvent is a flattened view of the permission-relevant fields of a
// rollout record.
type codexPermEvent struct {
	typ, id, name, namespace, arguments, input, output string
}

// collectPermEvents reads a rollout and returns its permission-relevant events
// plus lookup tables: escalated calls, calls with an output, and the cell ID
// each output reported.
func collectPermEvents(ctx context.Context, path string) (events []codexPermEvent, escCalls, outputs map[string]bool, callCell map[string]string) {
	escCalls = map[string]bool{}
	outputs = map[string]bool{}
	callCell = map[string]string{}
	_ = common.JSONLines(ctx, path, func(_ int, o map[string]any) error {
		p := common.Map(o["payload"])
		ev := codexPermEvent{
			typ:       common.String(p["type"]),
			id:        common.String(p["call_id"]),
			name:      common.String(p["name"]),
			namespace: common.String(p["namespace"]),
			arguments: common.String(p["arguments"]),
			input:     common.String(p["input"]),
			output:    common.Text(p["output"]),
		}
		if ev.typ == "" {
			ev.typ = common.String(o["type"])
		}
		events = append(events, ev)
		switch ev.typ {
		case "function_call", "local_shell_call", "custom_tool_call":
			if ev.id != "" && codexPermissionCall(ev) {
				escCalls[ev.id] = true
			}
		case "function_call_output", "local_shell_call_output", "custom_tool_call_output":
			if ev.id != "" {
				outputs[ev.id] = true
				if m := codexCellRE.FindStringSubmatch(ev.output); len(m) > 1 {
					callCell[ev.id] = m[1]
				}
			}
		}
		return nil
	})
	return events, escCalls, outputs, callCell
}

// permissionWait reports whether the session is currently blocked waiting for a
// permission/escalation approval.
func permissionWait(ctx context.Context, path string) bool {
	events, escCalls, outputs, callCell := collectPermEvents(ctx, path)

	// An escalated call with no output yet is still awaiting approval.
	for id := range escCalls {
		if !outputs[id] {
			return true
		}
	}

	// Escalated calls that resolved into a background cell: a later wait() poll
	// on one of these cells means we are still blocked on the escalation.
	escCells := map[string]bool{}
	for id := range escCalls {
		if cell := callCell[id]; cell != "" {
			escCells[cell] = true
		}
	}
	if len(escCells) == 0 {
		return false
	}
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		switch ev.typ {
		case "task_complete", "turn_aborted", "turn_ended":
			return false
		case "function_call":
			if ev.name == "wait" && !outputs[ev.id] {
				cell := codexWaitCell(ev.arguments)
				return cell != "" && escCells[cell]
			}
		}
	}
	return false
}

var codexCellRE = regexp.MustCompile(`cell ID (\d+)`)

// codexPermissionCall reports whether a call requires user approval: either an
// MCP tool invocation or an escalated-sandbox request.
func codexPermissionCall(ev codexPermEvent) bool {
	return strings.HasPrefix(ev.namespace, "mcp__") || strings.Contains(ev.arguments+" "+ev.input, "require_escalated")
}

// codexWaitCell extracts the cell_id argument from a wait() call, which may be
// encoded as either a string or a number.
func codexWaitCell(arguments string) string {
	var m map[string]any
	if json.Unmarshal([]byte(arguments), &m) != nil {
		return ""
	}
	switch v := m["cell_id"].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	default:
		return ""
	}
}

func (p *Plugin) PlanFork(_ context.Context, s domain.Session, t domain.ForkTarget) (domain.MutationPlan, domain.Command, error) {
	b, e := os.ReadFile(s.SourceRef.Source)
	if e != nil {
		return domain.MutationPlan{}, domain.Command{}, e
	}
	lines := bytes.Split(b, []byte("\n"))
	starts := codexTurnStarts(lines)
	total := len(starts)
	keep := t.KeepTurns
	if keep < 0 || keep > total {
		return domain.MutationPlan{}, domain.Command{}, fmt.Errorf("keep turns %d outside 0..%d", keep, total)
	}
	cut := len(lines)
	if keep < total {
		cut = starts[keep] // keep everything before the (keep+1)-th displayed turn
	}

	// Build a copy truncated at the chosen point as a new session (the original
	// is left untouched). Tag session_meta with forked_from_id so the manager
	// links the fork to its parent. Lines other than session_meta are copied
	// verbatim so the fork does not corrupt numbers or escaping.
	newID := common.NewID()
	var out bytes.Buffer
	for _, line := range lines[:cut] {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		o, e := common.UnmarshalJSONMap(line)
		if e != nil || common.String(o["type"]) != "session_meta" {
			out.Write(line)
			out.WriteByte('\n')
			continue
		}
		pl := common.Map(o["payload"])
		if pl == nil {
			pl = map[string]any{}
			o["payload"] = pl
		}
		pl["id"] = newID
		pl["forked_from_id"] = s.SessionID
		x, e := common.MarshalJSONLine(o)
		if e != nil {
			return domain.MutationPlan{}, domain.Command{}, e
		}
		out.Write(x)
	}
	path := filepath.Join(filepath.Dir(s.SourceRef.Source), "rollout-"+time.Now().UTC().Format("2006-01-02T15-04-05")+"-"+newID+".jsonl")
	plan := domain.MutationPlan{PluginID: p.id, Description: "fork Codex session", AllowedRoots: []string{p.o.SessionsDir}, Writes: []domain.FileWrite{{Path: path, Data: out.Bytes(), Mode: 0600}}}
	return plan, domain.Command{Executable: p.o.Executable, Args: []string{"resume", newID}, WorkingDirectory: s.CWD}, nil
}

// codexTurnStarts returns, in order, the line indexes at which each *displayed*
// turn of the rollout starts. It mirrors the parser/display rules exactly, so
// a KeepTurns computed from the rendered conversation truncates at the right
// line: queued user messages (the one following a turn_aborted, or repeats
// within an already-seen turn_id) do not start a turn, compaction summaries
// do (they are turn boundaries in TurnsOfPath), and thread_rolled_back drops
// the last num_turns user turns together with everything after them. A
// compaction record written in the middle of a turn's items is deferred to the
// next task_started (or EOF), matching the parser, so cutting at the compact
// turn keeps the interrupted turn's trailing output.
func codexTurnStarts(lines [][]byte) []int {
	type start struct {
		line int
		user bool // a real user turn (poppable by thread_rolled_back), not a compaction
	}
	var starts []start
	seenUserTurn := map[string]bool{}
	pendingAbort := false
	turnHasItems := false
	pendingCompacts := 0
	users := 0
	for i, line := range lines {
		var o map[string]any
		if json.Unmarshal(line, &o) != nil {
			continue
		}
		pl := common.Map(o["payload"])
		switch common.String(o["type"]) {
		case "compacted":
			if turnHasItems {
				pendingCompacts++
				continue
			}
			starts = append(starts, start{line: i})
		case "response_item":
			turnHasItems = true
			if !isCodexUserMessage(pl) {
				continue
			}
			turnID := codexTurnID(pl)
			if pendingAbort || (turnID != "" && seenUserTurn[turnID]) {
				pendingAbort = false
				continue // queued input on an existing/aborted turn
			}
			for ; pendingCompacts > 0; pendingCompacts-- {
				starts = append(starts, start{line: i}) // deferred compaction lands before the new turn
			}
			seenUserTurn[turnID] = true
			starts = append(starts, start{line: i, user: true})
			users++
		case "event_msg":
			switch common.String(pl["type"]) {
			case "task_started":
				for ; pendingCompacts > 0; pendingCompacts-- {
					starts = append(starts, start{line: i})
				}
				turnHasItems = false
			case "turn_aborted":
				pendingAbort = true
			case "thread_rolled_back":
				n := 0
				fmt.Sscan(fmt.Sprint(pl["num_turns"]), &n)
				if n <= 0 || n > users {
					continue
				}
				for j := len(starts) - 1; j >= 0; j-- {
					if starts[j].user {
						if n--; n == 0 {
							starts = starts[:j]
							break
						}
					}
				}
				users = 0
				for _, x := range starts {
					if x.user {
						users++
					}
				}
			}
		}
	}
	for ; pendingCompacts > 0; pendingCompacts-- {
		starts = append(starts, start{line: len(lines)})
	}
	out := make([]int, len(starts))
	for i, x := range starts {
		out[i] = x.line
	}
	return out
}

// isCodexUserMessage reports whether a payload is a real user message (a user
// chat message that is not an injected preamble or AGENTS.md instructions,
// matching what messageKind renders as a user event).
func isCodexUserMessage(pl map[string]any) bool {
	if common.String(pl["type"]) != "message" || common.String(pl["role"]) != "user" {
		return false
	}
	text := common.Text(pl["content"])
	return !codexIsPreamble(text) && !strings.Contains(text, "# AGENTS.md instructions")
}

func (p *Plugin) PlanRelocate(_ context.Context, old, new string, sessions []domain.Session) (domain.MutationPlan, error) {
	plan := domain.MutationPlan{PluginID: p.id, Description: "relocate Codex sessions", AllowedRoots: []string{p.o.SessionsDir}}
	for _, s := range sessions {
		if s.PluginID != p.id || s.CWD != old {
			continue
		}
		data, n, e := common.RewriteJSONL(s.SourceRef.Source, func(o map[string]any) bool {
			pl := common.Map(o["payload"])
			if common.String(pl["cwd"]) == old {
				pl["cwd"] = new
				return true
			}
			return false
		})
		if e != nil {
			return plan, e
		}
		if n > 0 {
			plan.Writes = append(plan.Writes, domain.FileWrite{Path: s.SourceRef.Source, Data: data, Mode: 0600})
		}
	}
	return plan, nil
}
