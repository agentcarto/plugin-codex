package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentcarto/core/domain"
)

func guardianFixture(t *testing.T, root, id, source, parent, body string) string {
	t.Helper()
	path := filepath.Join(root, "rollout-"+id+".jsonl")
	meta := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"thread_source":%q,"parent_thread_id":%q,"cwd":"/work"}}`+"\n", id, source, parent)
	if err := os.WriteFile(path, []byte(meta+body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const guardianRequest = `{"timestamp":"2026-09-16T02:41:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"copied parent transcript\n>>> APPROVAL REQUEST START\ncommand: read file\n>>> APPROVAL REQUEST END"}]}}
`

func TestGuardianResults(t *testing.T) {
	for _, tc := range []struct{ result, label string }{
		{"", "pending"},
		{`{\"outcome\":\"allow\",\"rationale\":\"read only\"}`, "allow"},
		{`{\"outcome\":\"deny\"}`, "deny"},
		{"unrecognized result", "unknown"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			body := guardianRequest
			if tc.result != "" {
				body += `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"` + tc.result + `"}]}}` + "\n"
			}
			path := guardianFixture(t, t.TempDir(), "review", "guardian_review", "parent", body)
			idx, err := indexGuardians(context.Background(), []string{path})
			if err != nil {
				t.Fatal(err)
			}
			reviews, err := readGuardianReviews(context.Background(), idx.files[path])
			if err != nil || len(reviews) != 1 {
				t.Fatalf("reviews=%+v err=%v", reviews, err)
			}
			e := reviews[0].event
			if e.Kind != domain.EventTask || e.ToolArg != "Auto review ["+tc.label+"]" ||
				strings.Contains(e.Text, "copied parent transcript") || !strings.Contains(e.Text, "read file") {
				t.Fatalf("bad review event: %+v", e)
			}
		})
	}
}

func TestGuardianIndexAndDependencies(t *testing.T) {
	root := t.TempDir()
	parent := guardianFixture(t, root, "parent", "user", "", "")
	review := guardianFixture(t, root, "review", "guardian_review", "parent", guardianRequest)
	idx, err := indexGuardians(context.Background(), []string{review, parent})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := idx.fingerprint(parent)
	f, err := os.OpenFile(review, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(guardianRequest)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := idx.fingerprint(parent); before == after {
		t.Fatal("review update did not invalidate parent")
	}
	reviews, err := readGuardianReviews(context.Background(), idx.files[review])
	if err != nil || len(reviews) != 2 || reviews[0].id == reviews[1].id {
		t.Fatalf("multiple review requests: %+v, %v", reviews, err)
	}
	if _, err := indexGuardians(context.Background(), []string{filepath.Join(root, "missing")}); err == nil {
		t.Fatal("read failure hidden")
	}
}

func TestFormatGuardianResult(t *testing.T) {
	input := `{"risk_level":"low","user_authorization":"high","outcome":"allow","rationale":"ローカルテストとビルド。\n外部送信なし。"}`
	status, got := formatGuardianResult(input)
	want := "Decision: Allowed\nRisk: Low\nUser authorization: High\n\nRationale:\nローカルテストとビルド。\n外部送信なし。"
	if status != "allow" || got != want {
		t.Fatalf("status=%q report=%q", status, got)
	}
	status, got = formatGuardianResult(`{"outcome":"deny"}`)
	if status != "deny" || got != "Decision: Denied" {
		t.Fatalf("partial decision: %q %q", status, got)
	}
	for _, raw := range []string{
		`not JSON`, `null`, `{"outcome":"defer"}`, `{"outcome":"allow","new_field":"keep me"}`,
		`{"outcome":"allow","risk_level":42}`,
	} {
		_, got := formatGuardianResult(raw)
		if got != raw {
			t.Fatalf("unknown data lost: %q -> %q", raw, got)
		}
	}
}
