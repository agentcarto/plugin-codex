package codex

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
)

// topicQuestion is one question as Codex puts it to the user, and twoQuestions a call asking it
// along with a second.
const (
	topicQuestion = `{"header":"Topic","id":"next_topic","question":"What should we work on next?","options":[` +
		`{"label":"Code (Recommended)","description":"look for things to improve"},` +
		`{"label":"Docs","description":"tighten the writing"}]}`
	twoQuestions = `{"questions":[` + topicQuestion + `,{"header":"Order","id":"order","question":"In which order?","options":[{"label":"By file","description":""}]}]}`
)

// askCall is the record of a request_user_input call with the given arguments, as Codex logs it:
// the arguments are a JSON document in a string.
func askCall(arguments string) string {
	return `{"timestamp":"2026-11-21T16:07:25Z","type":"response_item","payload":{"type":"function_call","status":"completed","id":"fc_1","name":"request_user_input","arguments":` +
		strconv.Quote(arguments) + `,"call_id":"call_1","internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}`
}

// askOutput is the record of that call's output, given as it stands in the record (a JSON value).
func askOutput(output string) string {
	return `{"timestamp":"2026-11-21T16:07:31Z","type":"response_item","payload":{"type":"function_call_output","id":"fco_1","call_id":"call_1","output":` +
		output + `,"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}`
}

// askRollout writes a rollout holding a prompt and the given records, and returns its path.
func askRollout(t *testing.T, dir, id string, records ...string) string {
	t.Helper()
	lines := append([]string{
		`{"timestamp":"2026-11-21T16:07:00Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"/work"}}`,
		`{"timestamp":"2026-11-21T16:07:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"ask me something"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}}`,
	}, records...)
	path := filepath.Join(dir, "rollout-2026-11-21T16-07-00-"+id+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// parseAsk parses a rollout of a prompt and the given records, and returns the events after the
// prompt.
func parseAsk(t *testing.T, records ...string) []domain.Event {
	t.Helper()
	ev, _, _, _, _, _ := parse(context.Background(), askRollout(t, t.TempDir(), "id", records...))
	if len(ev) == 0 || ev[0].Kind != domain.EventUser {
		t.Fatalf("events = %#v, want the prompt first", ev)
	}
	return ev[1:]
}

// What the agent asks the user is conversation: the readable question, in the turn it was asked
// in, with nothing of a tool call about it.
func TestParseAskCallIsAQuestion(t *testing.T) {
	ev := parseAsk(t, askCall(twoQuestions))

	want := "Topic: What should we work on next?\n" +
		"- Code (Recommended): look for things to improve\n" +
		"- Docs: tighten the writing\n" +
		"\n" +
		"Order: In which order?\n" +
		"- By file"
	if len(ev) != 1 || ev[0].Kind != domain.EventQuestion || ev[0].Text != want {
		t.Fatalf("got %#v, want a question reading:\n%s", ev, want)
	}
	if q := ev[0]; q.ToolName != "" || q.ToolArg != "" || q.ToolDetail != "" || q.TurnID != "turn-1" {
		t.Fatalf("a question should carry its turn and no tool fields: %#v", q)
	}
}

// A call that cannot be read whole (arguments that are no questions, a question without its id
// or text, an option without its label) stays what it was, a tool call with its raw arguments.
func TestParseAskCallWithoutReadableQuestionsStaysAToolCall(t *testing.T) {
	for _, arguments := range []string{
		`not json`,
		`{}`,
		`{"questions":"which?"}`,
		`{"questions":[{"header":"Topic","question":"What next?"}]}`,
		`{"questions":[{"header":"Topic","id":"next_topic"}]}`,
		`{"questions":[{"id":"next_topic","question":"What next?","options":[{"description":"look for things to improve"}]}]}`,
		`{"questions":[{"id":"next_topic","question":"What next?","options":["Code","Docs"]}]}`,
		`{"questions":[` + topicQuestion + `,{"header":"Order"}]}`,
	} {
		ev := parseAsk(t, askCall(arguments))
		if len(ev) != 1 || ev[0].Kind != domain.EventToolCall || ev[0].ToolName != "request_user_input" || ev[0].Text != arguments {
			t.Fatalf("%s: got %#v, want the tool call with its arguments", arguments, ev)
		}
	}
}

// What the user answered is conversation too: a line per question in the order asked, named by
// the question's header. What the user typed comes back as a "user_note: " entry: beside a
// choice it reads as a note, and with nothing picked it is the answer. A question the output
// leaves out was not answered.
func TestParseAskOutputIsTheAnswer(t *testing.T) {
	for _, c := range []struct{ arguments, output, want string }{
		{`{"questions":[` + topicQuestion + `]}`, `{"answers":{"next_topic":{"answers":["Code (Recommended)"]}}}`, "Topic: Code (Recommended)"},
		{`{"questions":[` + topicQuestion + `]}`, `{"answers":{"next_topic":{"answers":["None of the above","user_note: something else"]}}}`, "Topic: None of the above\n  note: something else"},
		{`{"questions":[` + topicQuestion + `]}`, `{"answers":{"next_topic":{"answers":["user_note: neither, drop the task"]}}}`, "Topic: neither, drop the task"},
		{`{"questions":[` + topicQuestion + `]}`, `{"answers":{}}`, "Topic: (no answer)"},
		{twoQuestions, `{"answers":{"order":{"answers":["By file"]}}}`, "Topic: (no answer)\nOrder: By file"},
	} {
		ev := parseAsk(t, askCall(c.arguments), askOutput(strconv.Quote(c.output)))
		if len(ev) != 2 || ev[0].Kind != domain.EventQuestion {
			t.Fatalf("%s: got %#v, want the question and its answer", c.output, ev)
		}
		a := ev[1]
		if a.Kind != domain.EventAnswer || a.Text != c.want {
			t.Fatalf("%s: got %#v, want an answer reading %q", c.output, a, c.want)
		}
		// An answer belongs to the turn its question was asked in.
		if a.ToolName != "" || a.ToolArg != "" || a.Prompt != "" || a.Command != "" || a.TurnID != "turn-1" {
			t.Fatalf("%s: an answer should carry its turn, no tool fields and open no turn: %#v", c.output, a)
		}
	}
}

// An output that is not the user's answers read whole stays a tool result: the tool's own errors
// and an interruption come as plain text, and answers of another shape are not guessed at.
func TestParseAskOutputNotReadWholeStaysAToolResult(t *testing.T) {
	for _, output := range []string{
		`request_user_input is unavailable in Default mode`,
		`aborted by user after 4.3s`,
		`{"answers":{"next_topic":{"answers":["Code (Recommended)"]}},"note":"typed in a hurry"}`,
		`{"answers":{"another_topic":{"answers":["Code (Recommended)"]}}}`,
		`{"answers":{"next_topic":"Code (Recommended)"}}`,
		`{"answers":{"next_topic":{"answers":["Code (Recommended)"],"other":"Tests"}}}`,
		`{"answers":{"next_topic":{"answers":[1]}}}`,
		`{"answers":["Code (Recommended)"]}`,
	} {
		ev := parseAsk(t, askCall(`{"questions":[`+topicQuestion+`]}`), askOutput(strconv.Quote(output)))
		if len(ev) != 2 || ev[0].Kind != domain.EventQuestion || ev[1].Kind != domain.EventToolResult || ev[1].Text != output {
			t.Fatalf("%s: got %#v, want the question and the tool result", output, ev)
		}
	}
	// An output that is not a string at all.
	ev := parseAsk(t, askCall(`{"questions":[`+topicQuestion+`]}`), askOutput(`{"answers":{"next_topic":{"answers":["Code (Recommended)"]}}}`))
	if len(ev) != 2 || ev[1].Kind != domain.EventToolResult {
		t.Fatalf("got %#v, want the tool result", ev)
	}
}

// A call left a tool call (its arguments could not be read) keeps its output a tool result, so
// the pair reads as it always did.
func TestParseOutputOfAnAskCallLeftAToolCallStaysAToolResult(t *testing.T) {
	output := `{"answers":{"next_topic":{"answers":["Code (Recommended)"]}}}`
	ev := parseAsk(t, askCall(`{"questions":"which?"}`), askOutput(strconv.Quote(output)))
	if len(ev) != 2 || ev[0].Kind != domain.EventToolCall || ev[1].Kind != domain.EventToolResult || ev[1].Text != output {
		t.Fatalf("got %#v, want the tool call and its result", ev)
	}
}

// The answer to a question marked secret is something the user typed in confidence. The question
// is shown; the answer stays a tool result, which is neither shown nor searched.
func TestParseAnswerToASecretAskStaysAToolResult(t *testing.T) {
	arguments := `{"questions":[{"header":"Token","id":"token","question":"Which token should the script use?","isSecret":true},` + topicQuestion + `]}`
	output := `{"answers":{"token":{"answers":["user_note: hunter2"]},"next_topic":{"answers":["Docs"]}}}`
	ev := parseAsk(t, askCall(arguments), askOutput(strconv.Quote(output)))
	if len(ev) != 2 || ev[0].Kind != domain.EventQuestion || !strings.HasPrefix(ev[0].Text, "Token: Which token should the script use?") {
		t.Fatalf("got %#v, want the question", ev)
	}
	if ev[1].Kind != domain.EventToolResult || ev[1].Text != output {
		t.Fatalf("got %#v, want the answers left a tool result", ev[1])
	}
}

// The conversation the host shows holds the question and the answer once each. Codex also logs a
// copy of the answer for its own use (retained_context), which is not read.
func TestLoadConversationShowsAnAskAndItsAnswerOnce(t *testing.T) {
	path := askRollout(t, t.TempDir(), "id",
		askCall(`{"questions":[`+topicQuestion+`]}`),
		`{"timestamp":"2026-11-21T16:07:31Z","type":"retained_context","payload":{"type":"verified_answer","turn_id":"turn-1","call_id":"call_1","questions":[{"question":"What should we work on next?\nCode (Recommended): look for things to improve","answer":"Code (Recommended)"}],"acceptance_order":12}}`,
		askOutput(strconv.Quote(`{"answers":{"next_topic":{"answers":["Code (Recommended)"]}}}`)),
	)
	c, err := (&Plugin{}).LoadConversation(context.Background(), domain.SessionRef{Source: path})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, id := range c.ActivePath() {
		for _, e := range c.Nodes[id].Events {
			got = append(got, string(e.Kind)+": "+e.Text)
		}
	}
	want := []string{
		"user: ask me something",
		"question: Topic: What should we work on next?\n- Code (Recommended): look for things to improve\n- Docs: tighten the writing",
		"answer: Topic: Code (Recommended)",
	}
	if strings.Join(got, "\n--\n") != strings.Join(want, "\n--\n") {
		t.Fatalf("conversation:\n%s\nwant:\n%s", strings.Join(got, "\n--\n"), strings.Join(want, "\n--\n"))
	}
}

// A session that stops at a question is waiting on the user as it was when the question was a
// tool call, and once the answer is in, the model is at work on it as after any tool result: the
// list reports what it always did.
func TestScanSessionEndingInAnAskReportsWhatItDid(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "11", "21")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	call := askCall(`{"questions":[` + topicQuestion + `]}`)
	askRollout(t, day, "asked", call)
	askRollout(t, day, "answered", call, askOutput(strconv.Quote(`{"answers":{"next_topic":{"answers":["Docs"]}}}`)))
	res, err := (&Plugin{id: "codex", o: Options{SessionsDir: root}}).Scan(context.Background(), plugin.ScanInput{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]domain.EventKind{}
	for _, s := range res.Sessions {
		got[s.SessionID] = s.LastKind
	}
	if got["asked"] != domain.EventToolCall || got["answered"] != domain.EventReasoning {
		t.Fatalf("LastKind = %v, want a tool in progress after the question and the model at work after the answer", got)
	}
}
