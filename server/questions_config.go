package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Clarification threads: when an agent asks the human questions
// (ask_user_question_kandev), the plugin posts them to Slack as a thread and
// turns an allowed person's reply into the answer. The settings are separate
// from triage so a mistake here never stops triage, and the other way round.

const (
	defaultQuestionPollSeconds = 10
	minQuestionPollSeconds     = 3
	maxQuestionPollSeconds     = 300
)

// questionsConfig is the validated clarification-thread configuration.
type questionsConfig struct {
	// KandevURL is where the plugin sends answers: Kandev's own REST route,
	// the same one the answer_question_kandev MCP tool resolves through.
	KandevURL string
	// KandevToken is an optional personal access token (kandev_pat_...). An
	// install with auth disabled needs none.
	KandevToken string
	// PublicURL, when set, turns the task title in the Slack post into a link.
	PublicURL string
	// DefaultChannel receives questions from workspaces with no mapping.
	DefaultChannel string
	// Channels maps a lower-cased workspace id or name to a channel id.
	Channels map[string]string
	// Answerers is the allow-list of Slack user ids whose replies count.
	Answerers    map[string]bool
	PollInterval time.Duration
}

// loadQuestionsConfig returns (nil, nil) when the feature is off. An error
// means the feature is on but cannot run; the caller reports it on the plugin
// page and keeps triage running.
func loadQuestionsConfig(raw map[string]any, mode authMode) (*questionsConfig, error) {
	if !configBool(raw, "questions_enabled") {
		return nil, nil
	}
	if mode != authModeApp {
		return nil, errors.New("clarification threads need the Slack app (Socket Mode): the browser-session fallback cannot receive thread replies")
	}
	q := &questionsConfig{
		KandevURL:      strings.TrimRight(strings.TrimSpace(configString(raw, "kandev_url")), "/"),
		KandevToken:    strings.TrimSpace(configString(raw, "kandev_api_token")),
		PublicURL:      strings.TrimRight(strings.TrimSpace(configString(raw, "kandev_public_url")), "/"),
		DefaultChannel: strings.TrimPrefix(strings.TrimSpace(configString(raw, "questions_default_channel")), "#"),
		Answerers:      map[string]bool{},
		PollInterval:   questionPollInterval(raw),
	}
	channels, err := parseChannelMap(configString(raw, "questions_channels"))
	if err != nil {
		return nil, err
	}
	q.Channels = channels
	for _, id := range parseChannels(configString(raw, "questions_answerers")) {
		q.Answerers[strings.TrimPrefix(strings.TrimSuffix(id, ">"), "<@")] = true
	}
	if err := q.validate(); err != nil {
		return nil, err
	}
	return q, nil
}

func (q *questionsConfig) validate() error {
	if q.KandevURL == "" {
		return errors.New("clarification threads need the Kandev URL the plugin answers through, e.g. http://127.0.0.1:3040")
	}
	parsed, err := url.Parse(q.KandevURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("the Kandev URL %q must be an http(s) URL such as http://127.0.0.1:3040", q.KandevURL)
	}
	if q.DefaultChannel == "" && len(q.Channels) == 0 {
		return errors.New("clarification threads need a default channel or a workspace → channel mapping")
	}
	if len(q.Answerers) == 0 {
		return errors.New("clarification threads need at least one allowed Slack user id (U…) whose replies answer the questions")
	}
	return nil
}

// channelFor picks the channel for a workspace: an explicit mapping by id,
// then by name, then the default. An empty result means nowhere to post.
func (q *questionsConfig) channelFor(workspaceID, workspaceName string) string {
	if ch := q.Channels[strings.ToLower(workspaceID)]; ch != "" {
		return ch
	}
	if name := strings.ToLower(strings.TrimSpace(workspaceName)); name != "" {
		if ch := q.Channels[name]; ch != "" {
			return ch
		}
	}
	return q.DefaultChannel
}

// parseChannelMap reads "workspace=channel" pairs separated by commas or new
// lines. The workspace side is an id or a name (names may contain spaces), and
// the channel side is a channel id; a leading # is tolerated.
func parseChannelMap(raw string) (map[string]string, error) {
	out := map[string]string{}
	entries := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == ';' })
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, channel, ok := strings.Cut(entry, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		channel = strings.TrimPrefix(strings.TrimSpace(channel), "#")
		if !ok || key == "" || channel == "" {
			return nil, fmt.Errorf("workspace → channel mapping %q must look like workspace=C0123456789", entry)
		}
		out[key] = channel
	}
	return out, nil
}

func questionPollInterval(raw map[string]any) time.Duration {
	seconds := configInt(raw, "questions_poll_seconds")
	if seconds == 0 {
		seconds = defaultQuestionPollSeconds
	}
	seconds = max(minQuestionPollSeconds, min(maxQuestionPollSeconds, seconds))
	return time.Duration(seconds) * time.Second
}
