package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agentcarto/core/common"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/scan"
)

type rolloutMeta struct {
	path, id, source, parent, fork string
	base                           *paginatedHistoryBase
}

type guardianIndex struct {
	files   map[string]rolloutMeta
	byID    map[string]string
	reviews map[string][]string
}

var errMetadataRead = errors.New("rollout metadata read")

func indexGuardians(ctx context.Context, paths []string) (*guardianIndex, error) {
	x := &guardianIndex{files: map[string]rolloutMeta{}, byID: map[string]string{}, reviews: map[string][]string{}}
	for _, path := range paths {
		m := rolloutMeta{path: path}
		err := common.JSONLines(ctx, path, func(_ int, record map[string]any) error {
			if common.String(record["type"]) != "session_meta" {
				return nil
			}
			p := common.Map(record["payload"])
			m.id, m.source = common.String(p["id"]), common.String(p["thread_source"])
			m.parent, m.fork = common.String(p["parent_thread_id"]), common.String(p["forked_from_id"])
			var err error
			m.base, err = historyBase(record)
			if err != nil {
				return err
			}
			return errMetadataRead
		})
		if err != nil && !errors.Is(err, errMetadataRead) {
			return nil, fmt.Errorf("index Codex rollout %s: %w", path, err)
		}
		x.files[path] = m
		if m.id != "" {
			if _, exists := x.byID[m.id]; exists {
				x.byID[m.id] = "" // An ambiguous parent cannot safely own a review.
			} else {
				x.byID[m.id] = path
			}
		}
		if m.source == "guardian_review" && m.parent != "" {
			x.reviews[m.parent] = append(x.reviews[m.parent], path)
		}
	}
	for id := range x.reviews {
		sort.Strings(x.reviews[id])
	}
	return x, nil
}

// dependencies includes ancestors because a fork can display their user turns.
// The conversation loader enforces history_base's ordinal boundary; review
// attachment below only uses turns actually present in that conversation.
func (x *guardianIndex) dependencies(path string) []string {
	seen := map[string]bool{}
	var visit func(string)
	visit = func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		m := x.files[path]
		for _, review := range x.reviews[m.id] {
			seen[review] = true
		}
		visit(x.byID[m.fork])
		if m.base != nil {
			visit(x.byID[m.base.threadID])
		}
	}
	visit(path)
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (x *guardianIndex) fingerprint(path string) (string, time.Time) {
	var parts []string
	var updated time.Time
	for _, p := range x.dependencies(path) {
		parts = append(parts, scan.Fingerprint(p))
		if t := common.FileTime(p); t.After(updated) {
			updated = t
		}
	}
	return strings.Join(parts, ":"), updated
}

type guardianReview struct {
	id, owner string
	requested time.Time
	event     domain.Event
}

func readGuardianReviews(ctx context.Context, m rolloutMeta) ([]guardianReview, error) {
	s := &codexParse{seenUserTurn: map[string]bool{}}
	if err := common.JSONLines(ctx, m.path, func(_ int, record map[string]any) error {
		s.consume(record)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read auto review %s: %w", m.path, err)
	}
	var out []guardianReview
	for _, e := range s.events {
		if isRealUserTurn(e) {
			id := fmt.Sprintf("%s/%d", m.id, len(out)+1)
			body := "Review: " + id + "\n"
			// Keep the requested action, not the copied parent transcript.
			if _, action, ok := strings.Cut(e.Text, ">>> APPROVAL REQUEST START"); ok {
				action, _, _ = strings.Cut(action, ">>> APPROVAL REQUEST END")
				body += strings.TrimSpace(action) + "\n"
			}
			out = append(out, guardianReview{id: id, owner: m.parent, requested: e.Timestamp,
				event: domain.Event{Kind: domain.EventTask, Timestamp: e.Timestamp,
					RawType: "guardian_review", ToolArg: "Auto review [pending]", ToolDetail: body, Text: body}})
		} else if e.Kind == domain.EventAssistant && len(out) > 0 {
			r := &out[len(out)-1]
			status, report := formatGuardianResult(e.Text)
			r.event.ToolArg = "Auto review [" + status + "]"
			r.event.ToolDetail = report + "\n\n" + r.event.ToolDetail
			r.event.Text, r.event.Model = r.event.ToolDetail, e.Model
		}
	}
	return out, nil
}

func formatGuardianResult(text string) (string, string) {
	var result map[string]string
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return "unknown", text
	}
	outcome := result["outcome"]
	decision := map[string]string{"allow": "Allowed", "deny": "Denied"}[outcome]
	if decision == "" {
		return "unknown", text
	}
	// Preserve new fields verbatim until their meaning and display are known.
	for key := range result {
		switch key {
		case "outcome", "risk_level", "user_authorization", "rationale":
		default:
			return outcome, text
		}
	}
	lines := []string{"Decision: " + decision + " (" + outcome + ")"}
	for _, field := range []struct{ key, label string }{
		{"risk_level", "Risk"}, {"user_authorization", "User authorization"},
	} {
		if value := result[field.key]; value != "" {
			label := map[string]string{"low": "Low", "medium": "Medium", "high": "High", "unknown": "Unknown"}[value]
			if label != "" {
				value = label + " (" + value + ")"
			}
			lines = append(lines, field.label+": "+value)
		}
	}
	if rationale := result["rationale"]; rationale != "" {
		lines = append(lines, "", "Rationale:", rationale)
	}
	return outcome, strings.Join(lines, "\n")
}
