package codex

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
)

func TestGuardianScanAndConversation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	parent := guardianFixture(t, root, "parent", "user", "", strings.ReplaceAll(guardianRequest, "02:41:01", "02:40:00"))
	review := guardianFixture(t, root, "review", "guardian_review", "parent", guardianRequest)
	p := &Plugin{id: "codex", o: Options{SessionsDir: root}}
	// Direct reading must work before Scan has built an index.
	c, err := p.LoadConversation(ctx, domain.SessionRef{Source: parent})
	if err != nil {
		t.Fatal(err)
	}
	count := func(c *domain.Conversation, label string) int {
		n := 0
		for _, node := range c.Nodes {
			for _, e := range node.Events {
				if e.Kind == domain.EventTask && strings.Contains(e.ToolArg, label) {
					n++
				}
			}
		}
		return n
	}
	if count(c, "pending") != 1 {
		t.Fatalf("missing pending review: %+v", c)
	}
	snap, err := p.Scan(ctx, plugin.ScanInput{})
	if err != nil || len(snap.Sessions) != 1 || snap.Dead[review] == "" {
		t.Fatalf("scan=%+v err=%v", snap, err)
	}
	f, err := os.OpenFile(review, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"{\"outcome\":\"allow\"}"}]}}` + "\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	updated, err := p.Scan(ctx, plugin.ScanInput{Warm: snap.Sessions, Dead: snap.Dead})
	if err != nil || len(updated.Sessions) != 1 || updated.Sessions[0].Fingerprint == snap.Sessions[0].Fingerprint {
		t.Fatalf("review update not reflected: %+v, %v", updated, err)
	}
	c, err = p.LoadConversation(ctx, domain.SessionRef{Source: parent})
	if err != nil || count(c, "allow") != 1 || count(c, "pending") != 0 {
		t.Fatalf("completion not reflected: %+v, %v", c, err)
	}
	expanded, err := p.Scan(ctx, plugin.ScanInput{IncludeInternal: true})
	if err != nil || len(expanded.Sessions) != 2 {
		t.Fatalf("explicit lookup missing review: %+v %v", expanded, err)
	}
	for _, s := range expanded.Sessions {
		if s.SessionID == "review" && !s.Unresumable {
			t.Fatal("internal review is resumable")
		}
	}
	second := guardianFixture(t, root, "review2", "guardian_review", "parent", guardianRequest)
	added, err := p.Scan(ctx, plugin.ScanInput{Warm: updated.Sessions, Dead: updated.Dead})
	if err != nil || len(added.Sessions) != 1 || added.Sessions[0].Fingerprint == updated.Sessions[0].Fingerprint {
		t.Fatalf("new review not detected: %+v %v", added, err)
	}
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	removed, err := p.Scan(ctx, plugin.ScanInput{Warm: added.Sessions, Dead: added.Dead})
	if err != nil || len(removed.Sessions) != 1 || removed.Sessions[0].Fingerprint != updated.Sessions[0].Fingerprint {
		t.Fatalf("review removal not reflected: %+v %v", removed, err)
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	orphan, err := p.Scan(ctx, plugin.ScanInput{Warm: updated.Sessions, Dead: updated.Dead})
	if err != nil || len(orphan.Sessions) != 1 || orphan.Sessions[0].SessionID != "review" {
		t.Fatalf("orphan review lost: %+v %v", orphan, err)
	}
}
