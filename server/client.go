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
	"strconv"
	"strings"
	"time"
)

// slackAPIBase is the single host for Slack's Web API. The same base serves
// every workspace — the token scopes the request. It is a var rather than a
// const only so the end-to-end tests can point the whole plugin at a stub
// Slack; nothing in production reassigns it.
var slackAPIBase = "https://slack.com/api"

const userAgent = "kandev-plugin-slack/0.1 (+https://github.com/kdlbs/kandev-plugin-slack)"

// maxResponseBytes bounds a single Slack response read. Slack's own limits
// are well under this; the cap exists so a proxy returning an unbounded body
// cannot exhaust the plugin subprocess.
const maxResponseBytes = 8 << 20

// requestTimeout bounds one Slack call. Generous enough for search.messages
// on a large workspace, short enough that a hung call cannot stall the poll
// loop past its own interval.
const requestTimeout = 30 * time.Second

// message is the minimal shape extracted from Slack for both the search and
// the thread fetch.
type message struct {
	TS        string `json:"ts"`
	ThreadTS  string `json:"threadTs,omitempty"`
	ChannelID string `json:"channelId"`
	UserID    string `json:"userId,omitempty"`
	UserName  string `json:"userName,omitempty"`
	Text      string `json:"text"`
	Permalink string `json:"permalink,omitempty"`
}

// sender renders the message author for the triage prompt and the reply.
func (m message) sender() string {
	switch {
	case m.UserName != "":
		return "@" + m.UserName
	case m.UserID != "":
		return "<@" + m.UserID + ">"
	default:
		return "the user"
	}
}

// authResult is the outcome of an auth.test probe.
type authResult struct {
	OK          bool   `json:"ok"`
	UserID      string `json:"userId,omitempty"`
	TeamID      string `json:"teamId,omitempty"`
	TeamName    string `json:"teamName,omitempty"`
	URL         string `json:"url,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Error       string `json:"error,omitempty"`
}

// apiError captures a Slack API failure. Slack answers 200 with
// `{"ok": false, "error": "..."}` for nearly every failure mode, so
// StatusCode is usually 200 and Message carries Slack's error string.
type apiError struct {
	StatusCode int
	Message    string
	RetryAfter int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("slack api: status %d: %s", e.StatusCode, e.Message)
}

// client speaks Slack's Web API with a bearer token and, in cookie mode, the
// browser `d` cookie alongside it.
type client struct {
	http     *http.Client
	endpoint string
	token    string
	cookie   string
	// mode is carried only so a rejected call can name the remedy that fits
	// the credential the operator actually pasted.
	mode authMode
}

func newClient(token, cookie string) *client {
	// The mode is inferred from the token rather than passed in so every call
	// site gets remedy text that matches the credential actually in use, even
	// the connection test on a half-filled form.
	mode := authModeApp
	if strings.HasPrefix(token, sessionTokenPrefix) {
		mode = authModeSession
	}
	return &client{
		http:     &http.Client{Timeout: requestTimeout},
		endpoint: slackAPIBase,
		token:    token,
		cookie:   cookie,
		mode:     mode,
	}
}

// envelope is the `{ok, error}` shape every Slack endpoint returns.
type envelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// post sends a form-encoded POST to /api/<method> and decodes the envelope.
// Slack expects form encoding, not JSON, for effectively every endpoint the
// plugin uses.
func (c *client) post(ctx context.Context, method string, params url.Values, out any) error {
	if c.token == "" {
		return errNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/"+method, strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.cookie != "" {
		req.Header.Set("Cookie", "d="+c.cookie)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return &apiError{StatusCode: resp.StatusCode, Message: "ratelimited", RetryAfter: max(0, retryAfter)}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{StatusCode: resp.StatusCode, Message: summarizeBody(raw)}
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return &apiError{StatusCode: resp.StatusCode, Message: "invalid Slack response: " + err.Error()}
	}
	if !env.OK {
		return &apiError{StatusCode: resp.StatusCode, Message: env.Error}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func summarizeBody(raw []byte) string {
	const maxMsg = 500
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "(empty body)"
	}
	if len(s) > maxMsg {
		return s[:maxMsg] + "…"
	}
	return s
}

// --- auth.test ---

type authTestResponse struct {
	envelope
	URL    string `json:"url"`
	Team   string `json:"team"`
	User   string `json:"user"`
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
}

// AuthTest is the cheapest call that confirms the credentials still work and
// returns the authenticated identity. A Slack-reported failure comes back as
// an authResult with OK=false rather than an error, so the caller can persist
// the reason; only transport failures surface as errors.
func (c *client) AuthTest(ctx context.Context) (*authResult, error) {
	var resp authTestResponse
	if err := c.post(ctx, "auth.test", url.Values{}, &resp); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			return &authResult{OK: false, Error: explainSlackError(apiErr.Message, c.mode)}, nil
		}
		return &authResult{OK: false, Error: err.Error()}, nil
	}
	return &authResult{
		OK:          true,
		UserID:      resp.UserID,
		TeamID:      resp.TeamID,
		TeamName:    resp.Team,
		URL:         resp.URL,
		DisplayName: resp.User,
	}, nil
}

// explainSlackError expands the Slack error codes an operator is most likely
// to hit while setting this plugin up. Slack's raw codes ("invalid_auth") give
// no hint about which of the three credential shapes is wrong, and the right
// remedy differs per mode — a stale `d` cookie is the usual cause for xoxc-
// and impossible for xoxb-/xoxp-, so the advice is mode-specific rather than
// listing every mode's fix and leaving the operator to pick.
func explainSlackError(code string, mode authMode) string {
	switch code {
	case "invalid_auth":
		return "invalid_auth — " + invalidAuthRemedy(mode)
	case "not_authed":
		return "not_authed — no credential reached Slack. Check the token field is actually saved."
	case "token_revoked":
		return "token_revoked — the token was revoked in Slack. " + reissueRemedy(mode)
	case "account_inactive":
		return "account_inactive — the authenticating Slack account is deactivated."
	case "missing_scope", "not_allowed_token_type":
		return code + " — the token lacks a scope this plugin needs. " + scopeRemedy(mode)
	case "ratelimited":
		return "ratelimited — Slack is throttling this workspace. Raise the poll interval."
	default:
		return code
	}
}

func invalidAuthRemedy(mode authMode) string {
	if mode == authModeSession {
		return "the credentials were rejected. This usually means the `d` cookie is stale — re-copy the token and the cookie from the same logged-in browser session."
	}
	return "the token was rejected. Reinstall the app in Slack and copy the current Bot User OAuth Token from OAuth & Permissions."
}

func reissueRemedy(mode authMode) string {
	if mode == authModeSession {
		return "Sign in to Slack again and re-copy the token and `d` cookie."
	}
	return "Reinstall the Slack app and copy the new token."
}

func scopeRemedy(mode authMode) string {
	if mode == authModeSession {
		return "The browser session must belong to an account that can see the channels you want triaged."
	}
	return "Re-apply the app manifest so the bot has app_mentions:read, channels:history, groups:history, chat:write, reactions:write and commands, then reinstall the app — scope changes only take effect on reinstall."
}

// --- search.messages ---

type searchMessagesResponse struct {
	envelope
	Messages struct {
		Matches []searchMatch `json:"matches"`
	} `json:"messages"`
}

type searchMatch struct {
	TS        string `json:"ts"`
	Text      string `json:"text"`
	User      string `json:"user"`
	Username  string `json:"username"`
	Permalink string `json:"permalink"`
	Channel   struct {
		ID string `json:"id"`
	} `json:"channel"`
	ThreadTS string `json:"thread_ts,omitempty"`
}

// searchPageSize is how many matches one search.messages call returns.
// Matches older than the watermark are discarded locally, so this only needs
// to cover the burst that can land inside one poll interval.
const searchPageSize = 30

// SearchMessages runs Slack's search.messages. Only user-scoped tokens
// (xoxp-, xoxc-) may call it; bot tokens get not_allowed_token_type.
func (c *client) SearchMessages(ctx context.Context, query string) ([]message, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("count", strconv.Itoa(searchPageSize))
	// Newest first; the watermark in the trigger discards anything already
	// processed, so a descending page is the cheapest way to stay current.
	params.Set("sort", "timestamp")
	params.Set("sort_dir", "desc")
	var resp searchMessagesResponse
	if err := c.post(ctx, "search.messages", params, &resp); err != nil {
		return nil, err
	}
	out := make([]message, 0, len(resp.Messages.Matches))
	for _, m := range resp.Messages.Matches {
		out = append(out, message{
			TS:        m.TS,
			ThreadTS:  m.ThreadTS,
			ChannelID: m.Channel.ID,
			UserID:    m.User,
			UserName:  m.Username,
			Text:      m.Text,
			Permalink: m.Permalink,
		})
	}
	return out, nil
}

// --- conversations.history / conversations.replies ---

type conversationsResponse struct {
	envelope
	Messages []conversationMessage `json:"messages"`
}

type conversationMessage struct {
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`
	User     string `json:"user"`
	Username string `json:"username"`
	Text     string `json:"text"`
	BotID    string `json:"bot_id,omitempty"`
	Subtype  string `json:"subtype,omitempty"`
}

// historyPageSize bounds one conversations.replies page. A thread longer than
// this is truncated to its first page rather than paged through: the agent's
// context is the real limit, not Slack's.
const historyPageSize = 100

// contextMessageLimit is how much of the surrounding channel conversation is
// gathered when a request is not anchored in a thread. A mention like
// "@Kandev file what Bob just said" is meaningless without the messages before
// it, and a request with no preceding context loses nothing by asking.
const contextMessageLimit = 20

// ConversationContext returns the conversation a request should be triaged
// against.
//
// A request inside a thread gets that thread. Anything else gets the recent
// channel history up to and including the triggering message — never past it,
// because messages that landed between detection and processing are unrelated
// to what the user asked for. A slash command has no triggering message, so
// its anchor is "now", which is the conversation the user was looking at when
// they ran it.
func (c *client) ConversationContext(ctx context.Context, channelID, threadTS, triggerTS string) ([]message, error) {
	if channelID == "" {
		return nil, errors.New("channel id required")
	}
	if threadTS != "" {
		return c.threadReplies(ctx, channelID, threadTS)
	}
	return c.recentHistory(ctx, channelID, triggerTS, contextMessageLimit)
}

// toMessages maps Slack's conversation shape onto the plugin's.
func toMessages(channelID string, in []conversationMessage) []message {
	out := make([]message, 0, len(in))
	for _, m := range in {
		out = append(out, message{
			TS:        m.TS,
			ThreadTS:  m.ThreadTS,
			ChannelID: channelID,
			UserID:    m.User,
			UserName:  m.Username,
			Text:      m.Text,
		})
	}
	return out
}

func (c *client) threadReplies(ctx context.Context, channelID, threadTS string) ([]message, error) {
	params := url.Values{}
	params.Set("channel", channelID)
	params.Set("ts", threadTS)
	params.Set("limit", strconv.Itoa(historyPageSize))
	var resp conversationsResponse
	if err := c.post(ctx, "conversations.replies", params, &resp); err != nil {
		return nil, err
	}
	return toMessages(channelID, resp.Messages), nil
}

// recentHistory reads the newest `limit` messages at or before latestTS. An
// empty latestTS means "up to now". Slack returns history newest-first, so the
// result is reversed into reading order before it reaches the prompt.
func (c *client) recentHistory(ctx context.Context, channelID, latestTS string, limit int) ([]message, error) {
	params := url.Values{}
	params.Set("channel", channelID)
	params.Set("limit", strconv.Itoa(limit))
	if latestTS != "" {
		params.Set("latest", latestTS)
		params.Set("inclusive", "true")
	}
	var resp conversationsResponse
	if err := c.post(ctx, "conversations.history", params, &resp); err != nil {
		return nil, err
	}
	out := make([]message, 0, len(resp.Messages))
	for i := len(resp.Messages) - 1; i >= 0; i-- {
		m := resp.Messages[i]
		// Joins, leaves and topic changes are noise. Messages from other apps
		// are kept deliberately — an alert posted by a monitoring bot is often
		// exactly the context the request is about.
		if m.Subtype != "" {
			continue
		}
		out = append(out, message{
			TS:        m.TS,
			ThreadTS:  m.ThreadTS,
			ChannelID: channelID,
			UserID:    m.User,
			UserName:  m.Username,
			Text:      m.Text,
		})
	}
	return out, nil
}

// --- chat.getPermalink ---

type permalinkResponse struct {
	envelope
	Permalink string `json:"permalink"`
}

// Permalink resolves a message's canonical Slack URL so the created task can
// link back to the conversation that produced it.
func (c *client) Permalink(ctx context.Context, channelID, ts string) (string, error) {
	params := url.Values{}
	params.Set("channel", channelID)
	params.Set("message_ts", ts)
	var resp permalinkResponse
	if err := c.post(ctx, "chat.getPermalink", params, &resp); err != nil {
		return "", err
	}
	return resp.Permalink, nil
}

// --- chat.postMessage / reactions.add ---

// PostMessage posts a reply. A non-empty threadTS keeps it in-thread.
func (c *client) PostMessage(ctx context.Context, channelID, threadTS, text string) error {
	_, _, err := c.PostMessageTS(ctx, channelID, threadTS, text)
	return err
}

// PostMessageTS posts like PostMessage and returns where the message landed:
// Slack's channel id (a name in the request comes back as an id) and the
// message ts, which is the thread_ts of any reply to it.
func (c *client) PostMessageTS(ctx context.Context, channelID, threadTS, text string) (string, string, error) {
	params := url.Values{}
	params.Set("channel", channelID)
	params.Set("text", text)
	if threadTS != "" {
		params.Set("thread_ts", threadTS)
	}
	var resp struct {
		envelope
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := c.post(ctx, "chat.postMessage", params, &resp); err != nil {
		return "", "", err
	}
	if resp.Channel == "" {
		resp.Channel = channelID
	}
	return resp.Channel, resp.TS, nil
}

// AddReaction adds an emoji reaction, given the bare name (no colons).
// Slack's `already_reacted` is swallowed so the caller can react
// idempotently across restarts and watermark retries.
func (c *client) AddReaction(ctx context.Context, channelID, ts, name string) error {
	params := url.Values{}
	params.Set("channel", channelID)
	params.Set("timestamp", ts)
	params.Set("name", name)
	err := c.post(ctx, "reactions.add", params, nil)
	if err == nil {
		return nil
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Message == "already_reacted" {
		return nil
	}
	return err
}

// compareTS orders Slack timestamps. They are fixed-format decimal strings
// ("1714659000.000100"), so lexicographic order matches chronological order
// and avoids a float parse that could lose the microsecond suffix.
func compareTS(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return -1
	case b == "":
		return 1
	case a < b:
		return -1
	default:
		return 1
	}
}

// RespondToCommand replies to a slash command through the response_url Slack
// issued with it. This is not the same channel as chat.postMessage: the URL is
// pre-authorized, so it works in channels the bot was never invited to, and an
// ephemeral response is visible only to the person who ran the command —
// matching how they invoked it.
//
// The URL is valid for 30 minutes and 5 uses, which comfortably covers one
// triage run.
func (c *client) RespondToCommand(ctx context.Context, responseURL, text string) error {
	if err := validateResponseURL(responseURL); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"response_type": "ephemeral",
		"text":          text,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{StatusCode: resp.StatusCode, Message: summarizeBody(raw)}
	}
	return nil
}

// validateResponseURL confirms the callback really points at Slack before the
// plugin posts to it. The URL arrives inside a payload, and a payload is data:
// treating it as a destination without checking would turn a malformed or
// spoofed frame into an outbound request to an arbitrary host.
func validateResponseURL(raw string) error {
	if raw == "" {
		return errors.New("no response_url on this command")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("unusable response_url: %w", err)
	}
	// The stub used by the end-to-end test is plain HTTP on localhost; the
	// scheme requirement only applies to the real Slack host.
	if parsed.Scheme != "https" && slackHostSuffix == "slack.com" {
		return fmt.Errorf("response_url must be https, got %q", parsed.Scheme)
	}
	if !isSlackHost(parsed.Hostname()) {
		return fmt.Errorf("response_url does not point at Slack: %q", parsed.Hostname())
	}
	return nil
}

// slackHostSuffix is the accepted response_url host. It is a var only so the
// end-to-end test can point delivery at its stub; production never changes it,
// and the rejection rules are covered directly against the default.
var slackHostSuffix = "slack.com"

func isSlackHost(host string) bool {
	return host == slackHostSuffix || strings.HasSuffix(host, "."+slackHostSuffix)
}
