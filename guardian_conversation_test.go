package codex

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

func TestGuardianAttachmentPreservesBranches(t *testing.T) {
	ts := func(n int64) time.Time { return time.Unix(n, 0) }
	u := func(n int64, text string) domain.Event {
		return domain.Event{Kind: domain.EventUser, Text: text, Timestamp: ts(n)}
	}
	nodes := []domain.ConvNode{
		{ID: "u1", Timestamp: ts(1), Events: []domain.Event{u(1, "first")}},
		{ID: "a1", Parent: "u1", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "reply", Timestamp: ts(2)}}},
		{ID: "abandoned", Parent: "a1", Timestamp: ts(3), Events: []domain.Event{u(3, "old turn")}},
		{ID: "current", Parent: "a1", Timestamp: ts(5), Events: []domain.Event{u(5, "new turn")}},
	}
	c := domain.NewConversation(nodes)
	path := c.ActivePath()
	reviews := []guardianReview{
		{requested: ts(2), event: domain.Event{Kind: domain.EventTask, Text: "allow"}},
		{requested: ts(4), event: domain.Event{Kind: domain.EventTask, Text: "deny"}},
		{event: domain.Event{Kind: domain.EventTask, Text: "unknown"}},
	}
	attachGuardianReviews(&c, domain.NewConversation(nodes), reviews, false)
	if len(c.Nodes) != len(nodes) || !reflect.DeepEqual(path, c.ActivePath()) {
		t.Fatal("review changed the tree or active branch")
	}
	if c.Nodes["u1"].Events[1].Text != "allow" || c.Nodes["abandoned"].Events[1].Text != "deny" {
		t.Fatal("review assigned to wrong turn or rollback branch")
	}
	if !strings.Contains(c.Nodes["current"].Events[1].ToolArg, "対応ターン不明") {
		t.Fatal("unknown timestamp presented as a certain match")
	}
	fork := domain.NewConversation(nodes[:2])
	attachGuardianReviews(&fork, domain.NewConversation(nodes), reviews[:2], true)
	if len(fork.Nodes["u1"].Events) != 2 || len(fork.Nodes["a1"].Events) != 1 {
		t.Fatal("fork lost its inherited review or gained a future review")
	}
}
