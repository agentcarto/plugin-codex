package codex

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/agentcarto/core/common"
	"github.com/agentcarto/core/domain"
)

func jsonObject(raw string) map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) == nil {
		return m
	}
	return nil
}

// This file normalizes Codex's tool-call payloads (exec commands, apply_patch
// documents, patch_apply_end changes) into the display fields the host renders
// generically (ToolArg/Changes).

// annotateTools fills the normalized display fields on tool and file-change
// events, in place.
func annotateTools(events []domain.Event) {
	for i := range events {
		e := &events[i]
		switch e.Kind {
		case domain.EventToolCall:
			if e.ToolName == "apply_patch" || strings.Contains(e.Text, "*** Begin Patch") {
				e.Changes = common.SplitPatch(patchText(e.Text))
				e.ToolArg = "apply_patch"
				continue
			}
			if e.RawType == "custom_tool_call" || (e.ToolName == "exec" && strings.Contains(e.Text, "tools.")) {
				e.ToolArg = execSummary(e.Text)
			}
		case domain.EventFileChange:
			e.Changes = common.SplitPatch(e.Text)
		}
	}
}

// patchText extracts the apply_patch document from a tool call's raw text: the
// text itself when it already is one, or the patch-bearing value of a JSON
// arguments object.
func patchText(raw string) string {
	if strings.Contains(raw, "*** Begin Patch") || strings.Contains(raw, "*** Add File:") || strings.Contains(raw, "*** Update File:") {
		return raw
	}
	if m := jsonObject(strings.TrimSpace(raw)); m != nil {
		for _, v := range m {
			if s, ok := v.(string); ok && strings.Contains(s, "*** ") {
				return s
			}
			if xs, ok := v.([]any); ok {
				parts := make([]string, 0, len(xs))
				for _, x := range xs {
					parts = append(parts, fmt.Sprint(x))
				}
				joined := strings.Join(parts, "\n")
				if strings.Contains(joined, "*** ") {
					return joined
				}
			}
		}
	}
	return raw
}

var codexCmdRE = regexp.MustCompile(`"?cmd"?\s*:\s*"((?:[^"\\]|\\.)*)"`)
var codexBatchCmdRE = regexp.MustCompile(`\[\s*"[^"]*"\s*,\s*"((?:[^"\\]|\\.)*)"\s*\]`)
var codexToolRE = regexp.MustCompile(`tools\.(\w+)`)

// execSummary condenses an exec/custom tool call's raw arguments into a
// one-line label: the first shell command (plus a "+N more" marker), the
// tools.* function name, or "apply_patch" for patch payloads.
func execSummary(raw string) string {
	if strings.Contains(raw, "*** Begin Patch") {
		return "apply_patch"
	}
	cmds := codexCmdRE.FindAllStringSubmatch(raw, -1)
	if len(cmds) == 0 {
		cmds = codexBatchCmdRE.FindAllStringSubmatch(raw, -1)
	}
	if len(cmds) > 0 {
		first := cmds[0][1]
		first = strings.ReplaceAll(first, `\n`, " ")
		first = strings.ReplaceAll(first, `\t`, " ")
		first = strings.ReplaceAll(first, `\"`, `"`)
		first = strings.ReplaceAll(first, `\\`, `\`)
		first = strings.Join(strings.Fields(first), " ")
		if len(cmds) > 1 {
			first += fmt.Sprintf("  (+%d more)", len(cmds)-1)
		}
		return first
	}
	if m := codexToolRE.FindStringSubmatch(raw); len(m) > 1 && m[1] != "exec_command" && m[1] != "exec" {
		return m[1]
	}
	return ""
}
