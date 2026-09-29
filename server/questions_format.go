package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// maxOptionsLettered bounds option letters to a..z. Kandev caps options far
// below this; the bound only keeps the letter arithmetic safe.
const maxOptionsLettered = 26

// formatQuestionPost renders a clarification bundle as the Slack thread's
// parent message: the task, any shared context, the numbered questions with
// lettered options, and how to answer.
func formatQuestionPost(task *pluginsdk.Task, taskLink string, in *pluginsdk.Interaction) string {
	var b strings.Builder
	b.WriteString(":question: *An agent needs your answer* — ")
	b.WriteString(taskLabel(task, taskLink))
	b.WriteString("\n")
	if c := strings.TrimSpace(in.Context); c != "" {
		b.WriteString("> ")
		b.WriteString(strings.ReplaceAll(slackEscape(c), "\n", "\n> "))
		b.WriteString("\n")
	}
	multi := len(in.Questions) > 1
	for i, q := range in.Questions {
		b.WriteString("\n")
		if multi {
			fmt.Fprintf(&b, "*%d.* ", i+1)
		}
		if t := strings.TrimSpace(q.Title); t != "" && t != strings.TrimSpace(q.Prompt) {
			b.WriteString("*" + slackEscape(t) + "* — ")
		}
		b.WriteString(slackEscape(strings.TrimSpace(q.Prompt)))
		b.WriteString("\n")
		for j, opt := range q.Options {
			if j >= maxOptionsLettered {
				break
			}
			fmt.Fprintf(&b, "      %c) %s", 'a'+j, slackEscape(opt.Label))
			if d := strings.TrimSpace(opt.Description); d != "" {
				b.WriteString(" — _" + slackEscape(d) + "_")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n_Reply in this thread to answer")
	if multi {
		b.WriteString(", one numbered line per question (`1. …`, `2. …`)")
	}
	b.WriteString(". An option letter or its exact text picks that option; anything else is sent as your own words._")
	return b.String()
}

func taskLabel(task *pluginsdk.Task, link string) string {
	title := "a Kandev task"
	if task != nil && strings.TrimSpace(task.Title) != "" {
		title = task.Title
	}
	label := slackEscape(title)
	if link != "" {
		label = "<" + link + "|" + strings.NewReplacer("|", "¦", ">", "›").Replace(label) + ">"
	} else {
		label = "*" + label + "*"
	}
	if task != nil && task.Identifier != "" {
		label += " (" + slackEscape(task.Identifier) + ")"
	}
	return label
}

// slackEscape escapes the three characters Slack's mrkdwn treats as control
// characters, so a question can never inject a mention or a link.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// slackUnescape reverses what Slack does to a user's message text.
func slackUnescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

// numberedLine matches "1. text", "1) text", "1: text" and "1 - text" at the
// start of a line, with optional bold markers Slack users add.
var numberedLine = regexp.MustCompile(`^\s*\*?(\d{1,2})\s*[.):\-]\*?\s*(.*)$`)

// errMissingAnswers is returned when a numbered reply skips a question.
// Kandev only accepts a bundle with every question answered.
type errMissingAnswers struct{ Missing []int }

func (e *errMissingAnswers) Error() string {
	parts := make([]string, len(e.Missing))
	for i, n := range e.Missing {
		parts[i] = fmt.Sprint(n)
	}
	return "no answer for question " + strings.Join(parts, ", ")
}

// parseReply maps a Slack reply onto one answer per question.
//
//   - One question: the whole reply is the answer.
//   - Several questions and numbered lines: each "N." line (plus the lines
//     below it) answers question N. A numbered reply that skips a question is
//     an error, because Kandev needs every question answered.
//   - Several questions and no numbering: the whole reply answers each one.
//
// Each answer text then becomes an option choice when it is an option letter
// ("b"), an option id, or an option's exact label; otherwise it is free text.
func parseReply(text string, questions []pluginsdk.InteractionQuestion) ([]pluginsdk.ClarificationAnswer, error) {
	text = strings.TrimSpace(slackUnescape(text))
	if text == "" {
		return nil, fmt.Errorf("the reply is empty")
	}
	if len(questions) == 0 {
		return nil, fmt.Errorf("the question has no items to answer")
	}
	texts := make([]string, len(questions))
	if len(questions) == 1 {
		texts[0] = text
	} else if numbered, ok := splitNumbered(text, len(questions)); ok {
		var missing []int
		for i := range questions {
			if strings.TrimSpace(numbered[i]) == "" {
				missing = append(missing, i+1)
			}
		}
		if len(missing) > 0 {
			return nil, &errMissingAnswers{Missing: missing}
		}
		texts = numbered
	} else {
		for i := range texts {
			texts[i] = text
		}
	}
	answers := make([]pluginsdk.ClarificationAnswer, len(questions))
	for i, q := range questions {
		answers[i] = answerFor(q, strings.TrimSpace(texts[i]))
	}
	return answers, nil
}

// splitNumbered collects the text under each "N." heading. It reports false
// when no line is numbered within 1..count, so an unnumbered reply falls back
// to answering every question with the whole text.
func splitNumbered(text string, count int) ([]string, bool) {
	out := make([]string, count)
	current := -1
	found := false
	for _, line := range strings.Split(text, "\n") {
		if m := numberedLine.FindStringSubmatch(line); m != nil {
			var n int
			_, _ = fmt.Sscanf(m[1], "%d", &n)
			if n >= 1 && n <= count {
				current = n - 1
				found = true
				out[current] = appendLine(out[current], m[2])
				continue
			}
		}
		if current >= 0 {
			out[current] = appendLine(out[current], line)
		}
	}
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out, found
}

func appendLine(existing, line string) string {
	if existing == "" {
		return line
	}
	return existing + "\n" + line
}

// answerFor turns one answer text into a choice or free text.
func answerFor(q pluginsdk.InteractionQuestion, text string) pluginsdk.ClarificationAnswer {
	answer := pluginsdk.ClarificationAnswer{QuestionID: q.ID}
	if id := matchOption(q.Options, text); id != "" {
		answer.SelectedOptions = []string{id}
		return answer
	}
	answer.CustomText = text
	return answer
}

func matchOption(options []pluginsdk.InteractionOption, text string) string {
	t := strings.ToLower(strings.TrimSpace(strings.Trim(text, "*_`")))
	t = strings.TrimSuffix(strings.TrimSuffix(t, ")"), ".")
	if t == "" {
		return ""
	}
	if len(t) == 1 && t[0] >= 'a' && t[0] <= 'z' {
		if idx := int(t[0] - 'a'); idx < len(options) && idx < maxOptionsLettered {
			return options[idx].OptionID
		}
	}
	for _, opt := range options {
		if strings.EqualFold(t, strings.TrimSpace(opt.OptionID)) || strings.EqualFold(t, strings.TrimSpace(opt.Label)) {
			return opt.OptionID
		}
	}
	return ""
}
