package codex

import (
	"sort"
	"time"

	"github.com/agentcarto/core/domain"
)

// attachGuardianReviews adds display-only events without changing node IDs,
// parent links, or the active leaf used for forks and rollback navigation.
func attachGuardianReviews(c *domain.Conversation, owner domain.Conversation, reviews []guardianReview, inherited bool) {
	type start struct {
		event domain.Event
	}
	var starts []start
	for _, n := range owner.Nodes {
		for _, e := range n.Events {
			if isRealUserTurn(e) {
				starts = append(starts, start{e})
			}
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].event.Timestamp.Before(starts[j].event.Timestamp) })
	// Inherited history can end in the middle of a turn. Only reviews requested
	// within the actually copied prefix belong to the fork.
	var inheritedEnd time.Time
	if inherited {
		present := map[string]bool{}
		for _, n := range c.Nodes {
			for _, e := range n.Events {
				present[guardianEventKey(e)] = true
			}
		}
		for _, n := range owner.Nodes {
			for _, e := range n.Events {
				if present[guardianEventKey(e)] && e.Timestamp.After(inheritedEnd) {
					inheritedEnd = e.Timestamp
				}
			}
		}
	}
	for _, review := range reviews {
		if inherited && (inheritedEnd.IsZero() || review.requested.After(inheritedEnd)) {
			continue
		}
		index := -1
		for i, s := range starts {
			if !s.event.Timestamp.IsZero() && !s.event.Timestamp.After(review.requested) {
				index = i
			}
		}
		target := ""
		if index >= 0 && (index == 0 || !starts[index-1].event.Timestamp.Equal(starts[index].event.Timestamp)) {
			key := guardianEventKey(starts[index].event)
			for id, n := range c.Nodes {
				for _, e := range n.Events {
					if guardianEventKey(e) == key {
						target = id
					}
				}
			}
		}
		if target == "" {
			if inherited {
				continue // The owner's turn is outside the inherited prefix.
			}
			target = c.ActiveLeaf
			review.event.ToolArg += " (対応ターン不明)"
		}
		if n, ok := c.Nodes[target]; ok {
			n.Events = append(n.Events, review.event)
			c.Nodes[target] = n
		}
	}
}

func guardianEventKey(e domain.Event) string {
	return string(e.Kind) + "\x00" + e.TurnID + "\x00" + e.Timestamp.Format(time.RFC3339Nano) + "\x00" + e.Text
}
