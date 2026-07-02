package codex

import "testing"

func TestPromptText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"fix the   bug\nplease", "fix the bug please"},
		{"<environment_context>cwd=/x</environment_context>", ""},
		{"<user_instructions>be terse</user_instructions>", ""},
		{"<turn_aborted>", ""},
		{"# AGENTS.md instructions\n...", ""},
		{"AGENTS.md instructions for repo", ""},
		{"/compact", ""},
		{"/compact but keep every design note from this long session", "/compact but keep every design note from this long session"},
		{"", ""},
	}
	for _, c := range cases {
		if got := promptText(c.in); got != c.want {
			t.Errorf("promptText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
