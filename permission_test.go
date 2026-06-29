package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPermissionWait(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	_ = os.WriteFile(p, []byte(`{"payload":{"type":"function_call","call_id":"x","name":"exec_command","arguments":"{\"sandbox_permissions\":\"require_escalated\"}"}}`+"\n"), 0600)
	if !permissionWait(context.Background(), p) {
		t.Fatal("expected waiting")
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString(`{"payload":{"type":"function_call_output","call_id":"x"}}` + "\n")
	_ = f.Close()
	if permissionWait(context.Background(), p) {
		t.Fatal("expected resolved")
	}
}

func TestPermissionWaitForEscalatedCellPoll(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	data := `{"payload":{"type":"function_call","call_id":"exec1","name":"exec_command","arguments":"{\"sandbox_permissions\":\"require_escalated\"}"}}
{"payload":{"type":"function_call_output","call_id":"exec1","output":"Script running with cell ID 16"}}
{"payload":{"type":"function_call","call_id":"wait1","name":"wait","arguments":"{\"cell_id\":16}"}}
`
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if !permissionWait(context.Background(), p) {
		t.Fatal("expected waiting while polling escalated cell")
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString(`{"payload":{"type":"task_complete"}}` + "\n")
	_ = f.Close()
	if permissionWait(context.Background(), p) {
		t.Fatal("expected task_complete to resolve waiting")
	}
}

func TestPermissionWaitForMCPNamespace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	data := `{"payload":{"type":"function_call","call_id":"mcp1","namespace":"mcp__github","name":"create_pull_request","arguments":"{}"}}
`
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if !permissionWait(context.Background(), p) {
		t.Fatal("expected mcp namespace call to wait for permission")
	}
}
