package codex

import (
	"testing"

	"github.com/agentcarto/core/domain"
)

func TestExecSummary(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"cmd":"ls -la"}`, "ls -la"},
		{`{"cmd":"a"} {"cmd":"b"}`, "a  (+1 more)"},
		{"*** Begin Patch\n*** End Patch", "apply_patch"},
		{`{"name":"tools.web_search"}`, "web_search"},
		{"nothing", ""},
	}
	for _, c := range cases {
		if got := execSummary(c.in); got != c.want {
			t.Errorf("execSummary(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAnnotateToolsPatchAndFileChange(t *testing.T) {
	events := []domain.Event{
		{Kind: domain.EventToolCall, ToolName: "apply_patch", Text: "*** Begin Patch\n*** Update File: a.go\n+x\n*** End Patch"},
		{Kind: domain.EventFileChange, Text: "*** Begin Patch\n*** Update File: a.go\n+x\n*** End Patch", RawType: "patch_apply_end"},
		{Kind: domain.EventToolCall, ToolName: "exec", RawType: "custom_tool_call", Text: `{"cmd":"make check"}`},
	}
	annotateTools(events)
	if len(events[0].Changes) != 1 || events[0].ToolArg != "apply_patch" {
		t.Fatalf("patch call=%+v", events[0])
	}
	if len(events[1].Changes) != 1 || events[1].Changes[0].Path != "a.go" {
		t.Fatalf("file change=%+v", events[1])
	}
	if events[2].ToolArg != "make check" {
		t.Fatalf("exec=%+v", events[2])
	}
}
