package codex

import (
	"strings"

	"github.com/agentcarto/core/common"
)

// This file reads Codex's ask tool, whose calls and outputs are conversation (EventQuestion,
// EventAnswer) rather than tool traffic.

// askTool is the tool Codex puts a question to the user with.
const askTool = "request_user_input"

// askedQuestion is what the answer to a question of an ask call is read with: the id it comes
// back under and the label that names the question there (common.Question.Label).
type askedQuestion struct{ id, label string }

// askQuestions reads an ask call from its arguments, a JSON document
// {"questions": [{id, header, question, isSecret, options: [{label, description}]}]}, as the Text
// of an EventQuestion and the questions its output answers. It returns "" and nil unless every
// question has its id and text and every option its label. A call is read whole or not at all:
// read in part, it would show less than the raw arguments it takes the place of.
//
// asked is nil as well when a question is marked secret. The answers to such a call are not
// read: left a tool result, a secret the user typed is neither printed in a transcript nor put
// in the search index or a summary's input, where an answer would be.
func askQuestions(arguments string) (text string, asked []askedQuestion) {
	var qs []common.Question
	secret := false
	for _, v := range common.Slice(jsonObject(arguments)["questions"]) {
		m := common.Map(v)
		id := common.String(m["id"])
		q := common.Question{Header: common.String(m["header"]), Text: common.String(m["question"])}
		options, listed := m["options"].([]any)
		if strings.TrimSpace(id) == "" || strings.TrimSpace(q.Text) == "" || (m["options"] != nil && !listed) {
			return "", nil
		}
		for _, ov := range options {
			o := common.Map(ov)
			opt := common.Option{Label: common.String(o["label"]), Description: common.String(o["description"])}
			if strings.TrimSpace(opt.Label) == "" {
				return "", nil
			}
			q.Options = append(q.Options, opt)
		}
		if marked, _ := m["isSecret"].(bool); marked {
			secret = true
		}
		qs = append(qs, q)
		asked = append(asked, askedQuestion{id: id, label: q.Label()})
	}
	if secret {
		asked = nil
	}
	return common.QuestionText(qs), asked
}

// askAnswer reads the output of an ask call as the Text of an EventAnswer. The answers are a JSON
// document in a string, {"answers": {question id: {"answers": [label, "user_note: text"]}}}: a
// question left out was not answered, and a "user_note: " entry is what the user typed, beside
// the choice or, with no label, in place of one. It returns "" for an output that is not that
// document, whole: the tool's own errors and an interruption come as plain text, which is no
// response of the user's, and answers of another shape are not guessed at. Such an output stays
// a tool result.
func askAnswer(output any, asked []askedQuestion) string {
	raw, _ := output.(string)
	doc := jsonObject(raw)
	given, ok := doc["answers"].(map[string]any)
	if !ok || len(doc) != 1 {
		return ""
	}
	answers := make([]common.Answer, 0, len(asked))
	read := 0
	for _, q := range asked {
		answer := common.Answer{Header: q.label}
		if v, answered := given[q.id]; answered {
			entry := common.Map(v)
			values, listed := entry["answers"].([]any)
			if !listed || len(entry) != 1 {
				return ""
			}
			var notes []string
			for _, v := range values {
				value, ok := v.(string)
				if !ok {
					return ""
				}
				if note, isNote := strings.CutPrefix(value, "user_note: "); isNote {
					notes = append(notes, note)
				} else {
					answer.Values = append(answer.Values, value)
				}
			}
			if len(answer.Values) == 0 {
				// Nothing picked: what the user typed is the answer, not a note on one.
				answer.Values, notes = notes, nil
			}
			answer.Note = strings.Join(notes, "\n")
			read++
		}
		answers = append(answers, answer)
	}
	if read != len(given) {
		return ""
	}
	return common.AnswerText(answers)
}
