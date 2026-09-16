package codex

import (
	"context"
	"path/filepath"

	"github.com/agentcarto/core/common"
	"github.com/agentcarto/core/domain"
)

func (p *Plugin) loadGuardianEvents(ctx context.Context, source string, c *domain.Conversation) error {
	p.indexMu.RLock()
	idx := p.guardians
	p.indexMu.RUnlock()
	if idx == nil {
		root := p.o.SessionsDir
		if root == "" {
			root = filepath.Dir(source)
		}
		paths, err := common.WalkFiles(root, isRolloutFile)
		if err != nil {
			return err
		}
		idx, err = indexGuardians(ctx, paths)
		if err != nil {
			return err
		}
	}
	if idx.files[source].source == "guardian_review" {
		return nil // Explicit lookup keeps the review's full original history.
	}
	owners := map[string]domain.Conversation{}
	for _, path := range idx.dependencies(source) {
		m := idx.files[path]
		if m.source != "guardian_review" {
			continue
		}
		reviews, err := readGuardianReviews(ctx, m)
		if err != nil {
			return err
		}
		ownerPath := idx.byID[m.parent]
		owner, ok := owners[ownerPath]
		if !ok {
			parsed, err := p.parseConversationRollout(ctx, ownerPath)
			if err != nil {
				return err
			}
			owner = rollbackConversation(withoutSyntheticStatus(parsed.events), "thread_rolled_back")
			owners[ownerPath] = owner
		}
		attachGuardianReviews(c, owner, reviews, ownerPath != source)
	}
	return nil
}
