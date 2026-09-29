package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// Why the plugin answers over HTTP instead of the Host API:
//
// The plugin Host API can read pending questions (api_read: interactions), but
// it refuses to answer one unless the request carries a "human response
// receipt" that only Kandev's logged-in web UI can mint. A reply typed in Slack
// never passes through that UI, so the Host API cannot carry it.
//
// Kandev's REST route POST /api/v1/clarification/{pending_id}/respond runs the
// same Resolver as the answer_question_kandev MCP tool: the same validation,
// the same "first answer wins" claim, the same resume of the waiting agent. The
// operator authorises it by giving the plugin a personal access token, and the
// plugin only uses it for a reply from an allow-listed Slack user.

// answerOutcome classifies Kandev's reply to an answer.
type answerOutcome int

const (
	// answerApplied: this answer won and the agent resumes.
	answerApplied answerOutcome = iota
	// answerAlreadyResolved: someone answered first, or the question was
	// cancelled or expired. Nothing more to do in Slack but say so.
	answerAlreadyResolved
	// answerGone: Kandev does not know this pending id any more.
	answerGone
	// answerRejected: Kandev refused the answer itself (for example an
	// unknown option). The question stays open; the person can reply again.
	answerRejected
	// answerFailed: Kandev could not be reached or failed. Retryable.
	answerFailed
)

// answerResult is the classified outcome plus Kandev's own message.
type answerResult struct {
	Outcome answerOutcome
	Detail  string
}

// questionAnswerer is the seam the bridge uses to submit answers, so tests can
// swap Kandev for a fake.
type questionAnswerer interface {
	Answer(ctx context.Context, pendingID string, answers []pluginsdk.ClarificationAnswer) answerResult
}

// kandevAPI answers through Kandev's REST route.
type kandevAPI struct {
	base  string
	token string
	http  *http.Client
}

func newKandevAPI(base, token string) *kandevAPI {
	return &kandevAPI{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: requestTimeout}}
}

type respondAnswer struct {
	QuestionID      string   `json:"question_id"`
	SelectedOptions []string `json:"selected_options,omitempty"`
	CustomText      string   `json:"custom_text,omitempty"`
}

type respondBody struct {
	Answers []respondAnswer `json:"answers"`
}

// Answer posts the bundle's answers and classifies the reply.
func (k *kandevAPI) Answer(ctx context.Context, pendingID string, answers []pluginsdk.ClarificationAnswer) answerResult {
	body := respondBody{Answers: make([]respondAnswer, len(answers))}
	for i, a := range answers {
		body.Answers[i] = respondAnswer{QuestionID: a.QuestionID, SelectedOptions: a.SelectedOptions, CustomText: a.CustomText}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return answerResult{Outcome: answerFailed, Detail: err.Error()}
	}
	endpoint := k.base + "/api/v1/clarification/" + url.PathEscape(pendingID) + "/respond"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return answerResult{Outcome: answerFailed, Detail: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return answerResult{Outcome: answerFailed, Detail: "cannot reach Kandev: " + redactURLError(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	return classifyRespond(resp.StatusCode, payload)
}

// classifyRespond maps the REST envelope documented on
// clarification.writeResolutionResult: 200 with claimed true/false, 404 for an
// unknown or hidden bundle, 409 when it is no longer active, 400 for a
// validation error, 503 while Kandev is busy, 401/403 for credentials.
func classifyRespond(status int, payload []byte) answerResult {
	var envelope struct {
		Claimed bool   `json:"claimed"`
		Status  string `json:"status"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(payload, &envelope)
	detail := envelope.Error
	switch {
	case status == http.StatusOK && envelope.Claimed:
		return answerResult{Outcome: answerApplied}
	case status == http.StatusOK:
		return answerResult{Outcome: answerAlreadyResolved, Detail: "it was already " + orDefault(envelope.Status, "answered")}
	case status == http.StatusNotFound:
		return answerResult{Outcome: answerGone, Detail: orDefault(detail, "not found")}
	case status == http.StatusConflict:
		return answerResult{Outcome: answerAlreadyResolved, Detail: orDefault(detail, "no longer active")}
	case status == http.StatusBadRequest:
		return answerResult{Outcome: answerRejected, Detail: orDefault(detail, "invalid answer")}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return answerResult{Outcome: answerFailed, Detail: fmt.Sprintf("Kandev refused the plugin's API token (HTTP %d) — check the Kandev API token setting", status)}
	default:
		return answerResult{Outcome: answerFailed, Detail: fmt.Sprintf("Kandev answered HTTP %d: %s", status, orDefault(detail, summarizeBody(payload)))}
	}
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// redactURLError keeps a transport error readable without echoing a URL that
// could, in a misconfiguration, carry credentials.
func redactURLError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err.Error()
	}
	return err.Error()
}
