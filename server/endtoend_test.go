package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// These tests drive the whole plugin — config, Slack calls, agent, task
// creation, reply, watermark — against a stub Slack and a fake Host. The unit
// tests above cover the pieces; these cover the wiring between them, which is
// where the session fallback's regressions lived.

// --- fake Host ---

type fakeHost struct {
	pluginsdk.Host

	config map[string]any

	mu      sync.Mutex
	state   map[string]map[string]any
	created []pluginsdk.CreateTaskInput
	prompts []string

	// agentResponse is what InvokeUtilityAgent returns; agentErr overrides it.
	agentResponse string
	agentErr      error
	createErr     error
}

func newFakeHost(config map[string]any) *fakeHost {
	return &fakeHost{
		config: config,
		state:  map[string]map[string]any{},
		agentResponse: `{"workspace_id":"ws-1","workflow_id":"wf-1","column":"Backlog",
			"title":"Fix SSO login redirect loop","description":"From Slack.","reply":"Filed it."}`,
	}
}

func (h *fakeHost) GetConfig(context.Context) (map[string]any, error) { return h.config, nil }

func (h *fakeHost) GetState(_ context.Context, _, _, key string) (map[string]any, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.state[key]
	return v, ok, nil
}

func (h *fakeHost) SetState(_ context.Context, _, _, key string, value map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state[key] = value
	return nil
}

func (h *fakeHost) InvokeUtilityAgent(_ context.Context, prompt string, _ ...pluginsdk.UtilityAgentOptions) (string, error) {
	h.mu.Lock()
	h.prompts = append(h.prompts, prompt)
	h.mu.Unlock()
	if h.agentErr != nil {
		return "", h.agentErr
	}
	return h.agentResponse, nil
}

func (h *fakeHost) Workspaces() pluginsdk.WorkspaceReader    { return fakeWorkspaces{} }
func (h *fakeHost) Workflows() pluginsdk.WorkflowReader      { return fakeWorkflows{} }
func (h *fakeHost) Repositories() pluginsdk.RepositoryReader { return fakeRepositories{} }
func (h *fakeHost) Tasks() pluginsdk.TaskReader              { return &fakeTasks{host: h} }

func (h *fakeHost) createdTasks() []pluginsdk.CreateTaskInput {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]pluginsdk.CreateTaskInput(nil), h.created...)
}

func (h *fakeHost) watermark() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	marks, ok := h.state[stateWatermark]
	if !ok {
		return ""
	}
	ts, _ := marks[searchWatermarkKey].(string)
	return ts
}

type fakeWorkspaces struct{ pluginsdk.WorkspaceReader }

func (fakeWorkspaces) List(context.Context, pluginsdk.Page) ([]pluginsdk.Workspace, *pluginsdk.PageInfo, error) {
	return []pluginsdk.Workspace{{ID: "ws-1", Name: "Platform"}}, nil, nil
}

type fakeWorkflows struct{ pluginsdk.WorkflowReader }

func (fakeWorkflows) List(context.Context, string, pluginsdk.Page) ([]pluginsdk.Workflow, *pluginsdk.PageInfo, error) {
	return []pluginsdk.Workflow{{ID: "wf-1", WorkspaceID: "ws-1", Name: "Engineering"}}, nil, nil
}

func (fakeWorkflows) ListSteps(context.Context, string) ([]pluginsdk.WorkflowStep, error) {
	return []pluginsdk.WorkflowStep{
		{ID: "step-backlog", WorkflowID: "wf-1", Name: "Backlog"},
		{ID: "step-doing", WorkflowID: "wf-1", Name: "In Progress"},
	}, nil
}

type fakeRepositories struct{ pluginsdk.RepositoryReader }

func (fakeRepositories) List(context.Context, string, pluginsdk.Page) ([]pluginsdk.Repository, *pluginsdk.PageInfo, error) {
	return []pluginsdk.Repository{{ID: "repo-1", WorkspaceID: "ws-1", Name: "kandev"}}, nil, nil
}

type fakeTasks struct {
	pluginsdk.TaskReader
	host *fakeHost
}

func (f *fakeTasks) Create(_ context.Context, in pluginsdk.CreateTaskInput) (*pluginsdk.Task, error) {
	if f.host.createErr != nil {
		return nil, f.host.createErr
	}
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	f.host.created = append(f.host.created, in)
	return &pluginsdk.Task{ID: "task-1", Title: in.Title, Identifier: "PLAT-482"}, nil
}

// --- stub Slack ---

type stubSlack struct {
	srv *httptest.Server

	mu    sync.Mutex
	calls map[string]int
	posts []string
	// commandResponses records bodies delivered to a slash command's
	// response_url, which is a different route from chat.postMessage.
	commandResponses []string
	// searchBody is returned for search.messages.
	searchBody string
}

func newStubSlack(t *testing.T, searchBody string) *stubSlack {
	t.Helper()
	s := &stubSlack{calls: map[string]int{}, searchBody: searchBody}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/commands/") {
			body, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			s.commandResponses = append(s.commandResponses, string(body))
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/")
		_ = r.ParseForm()
		s.mu.Lock()
		s.calls[method]++
		if method == "chat.postMessage" {
			s.posts = append(s.posts, r.PostForm.Get("text"))
		}
		body := s.searchBody
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"team":"Acme","user":"alice","team_id":"T1","user_id":"U1"}`))
		case "search.messages":
			_, _ = w.Write([]byte(body))
		case "conversations.replies", "conversations.history":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"ts":"2.0","user":"U1","text":"!kandev fix sso"}]}`))
		case "chat.getPermalink":
			_, _ = w.Write([]byte(`{"ok":true,"permalink":"https://acme.slack.com/p2"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(s.srv.Close)

	// Point the whole plugin at the stub for the duration of the test.
	previous := slackAPIBase
	slackAPIBase = s.srv.URL
	t.Cleanup(func() { slackAPIBase = previous })
	return s
}

func (s *stubSlack) callCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

func (s *stubSlack) postedTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.posts...)
}

func (s *stubSlack) commandReplies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commandResponses...)
}

// responseURL points at the stub while still satisfying the Slack-host check,
// so the validation and the delivery are both exercised.
func (s *stubSlack) responseURL() string {
	return s.srv.URL + "/commands/T1/1/abc"
}

func sessionRawConfig() map[string]any {
	return map[string]any{
		"session_token":  "xoxc-test",
		"session_cookie": "d-test",
		"utility_agent":  "agent-1",
	}
}

const oneMatch = `{"ok":true,"messages":{"matches":[
	{"ts":"2.0","text":"!kandev fix sso","user":"U1","username":"alice","channel":{"id":"C1"}}]}}`

// The whole point of the fallback: a browser-session install with no Slack app
// must still turn a `!kandev` message into a task and reply in-thread.
func TestSessionFallbackCreatesATaskEndToEnd(t *testing.T) {
	slack := newStubSlack(t, oneMatch)
	host := newFakeHost(sessionRawConfig())
	s := newSupervisor(func() pluginsdk.Host { return host })

	s.tick(context.Background(), true)

	created := host.createdTasks()
	if len(created) != 1 {
		t.Fatalf("created %d tasks, want 1", len(created))
	}
	if created[0].Title != "Fix SSO login redirect loop" {
		t.Fatalf("title = %q", created[0].Title)
	}
	if created[0].WorkspaceID != "ws-1" || created[0].WorkflowID != "wf-1" {
		t.Fatalf("task landed at %+v, want ws-1/wf-1", created[0])
	}
	if created[0].WorkflowStepID == nil || *created[0].WorkflowStepID != "step-backlog" {
		t.Fatalf("step = %v, want step-backlog", created[0].WorkflowStepID)
	}
	posts := slack.postedTexts()
	if len(posts) != 1 || !strings.Contains(posts[0], "Filed it.") || !strings.Contains(posts[0], "PLAT-482") {
		t.Fatalf("reply = %v, want the agent's text plus the task identifier", posts)
	}
	if slack.callCount("reactions.add") != 1 {
		t.Fatalf("reactions.add called %d times, want 1", slack.callCount("reactions.add"))
	}
	if host.watermark() != "2.0" {
		t.Fatalf("watermark = %q, want it advanced to 2.0", host.watermark())
	}
}

// The watermark is a single high-water mark, so advancing it past a message
// that never became a task drops that request permanently.
func TestSessionFallbackKeepsTheWatermarkWhenTriageFails(t *testing.T) {
	newStubSlack(t, oneMatch)
	host := newFakeHost(sessionRawConfig())
	host.agentErr = errors.New("agent unavailable")
	s := newSupervisor(func() pluginsdk.Host { return host })

	s.tick(context.Background(), true)

	if len(host.createdTasks()) != 0 {
		t.Fatal("no task should exist when the agent failed")
	}
	if host.watermark() != "" {
		t.Fatalf("watermark = %q, want it held back so the next scan retries", host.watermark())
	}
}

// A failed request must be retryable. The in-flight claim that stops a
// duplicate has to be given back, or the retry above is silently discarded.
func TestSessionFallbackRetriesAfterAFailure(t *testing.T) {
	newStubSlack(t, oneMatch)
	host := newFakeHost(sessionRawConfig())
	host.agentErr = errors.New("agent unavailable")
	s := newSupervisor(func() pluginsdk.Host { return host })

	s.tick(context.Background(), true)
	if len(host.createdTasks()) != 0 {
		t.Fatal("first pass should have failed")
	}

	// The agent recovers; the same message must be picked up again.
	host.agentErr = nil
	s.tick(context.Background(), true)

	if len(host.createdTasks()) != 1 {
		t.Fatalf("created %d tasks after recovery, want the retry to succeed", len(host.createdTasks()))
	}
	if host.watermark() != "2.0" {
		t.Fatalf("watermark = %q, want it advanced once the retry succeeded", host.watermark())
	}
}

// Re-finding the same message after a successful scan must not create a second
// task, even before the watermark write lands.
func TestSessionFallbackDoesNotDuplicateOnRescan(t *testing.T) {
	newStubSlack(t, oneMatch)
	host := newFakeHost(sessionRawConfig())
	s := newSupervisor(func() pluginsdk.Host { return host })

	s.tick(context.Background(), true)
	s.tick(context.Background(), true)

	if len(host.createdTasks()) != 1 {
		t.Fatalf("created %d tasks across two scans, want 1", len(host.createdTasks()))
	}
}

// Sharing the probe fingerprint with the source lifecycle made the fallback
// re-probe on every scan, since the source is torn down each tick.
func TestSessionFallbackProbesOncePerInterval(t *testing.T) {
	slack := newStubSlack(t, `{"ok":true,"messages":{"matches":[]}}`)
	host := newFakeHost(sessionRawConfig())
	s := newSupervisor(func() pluginsdk.Host { return host })

	for range 3 {
		s.tick(context.Background(), true)
	}
	if got := slack.callCount("auth.test"); got != 1 {
		t.Fatalf("auth.test called %d times across 3 scans, want 1 until the probe interval elapses", got)
	}
}

// The fallback exists for workspaces that forbid app installs, so its own
// credentials must select it even though the app path is preferred generally.
func TestSessionFallbackIsSelectedWithoutAppTokens(t *testing.T) {
	newStubSlack(t, `{"ok":true,"messages":{"matches":[]}}`)
	host := newFakeHost(sessionRawConfig())
	s := newSupervisor(func() pluginsdk.Host { return host })

	s.tick(context.Background(), true)

	raw, ok, _ := host.GetState(context.Background(), stateScope, "", stateStatus)
	if !ok {
		t.Fatal("no status was written")
	}
	st := statusFromMap(raw)
	if st.Mode != string(authModeSession) || !st.Configured || !st.OK {
		t.Fatalf("status = %+v, want a healthy session fallback", st)
	}
	if st.TeamName != "Acme" {
		t.Fatalf("team = %q, want the probed identity", st.TeamName)
	}
}

// A Socket Mode request runs the same triage path, so the fallback and the app
// cannot drift apart in what they produce.
func TestSocketRequestUsesTheSameTriagePath(t *testing.T) {
	slack := newStubSlack(t, "")
	host := newFakeHost(map[string]any{
		"app_token": "xapp-1-test", "bot_token": "xoxb-test", "utility_agent": "agent-1",
	})
	cfg, err := loadConfig(host.config)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	r := newRunner(func() pluginsdk.Host { return host })

	err = r.Handle(context.Background(), cfg, inboundRequest{
		ChannelID: "C1", TS: "2.0", UserID: "U1",
		Text: "<@U0BOT> fix sso", Instruction: "fix sso", Acknowledge: true,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(host.createdTasks()) != 1 {
		t.Fatalf("created %d tasks, want 1", len(host.createdTasks()))
	}
	if len(slack.postedTexts()) != 1 {
		t.Fatalf("posted %d replies, want 1", len(slack.postedTexts()))
	}
}

// Slack redelivers unacknowledged envelopes and replays on reconnect; neither
// may produce a second task.
func TestSocketRedeliveryIsDeduplicated(t *testing.T) {
	newStubSlack(t, "")
	host := newFakeHost(map[string]any{
		"app_token": "xapp-1-test", "bot_token": "xoxb-test", "utility_agent": "agent-1",
	})
	cfg, _ := loadConfig(host.config)
	r := newRunner(func() pluginsdk.Host { return host })
	req := inboundRequest{ChannelID: "C1", TS: "2.0", Instruction: "fix sso", Acknowledge: true}

	for range 3 {
		if err := r.Handle(context.Background(), cfg, req); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	if len(host.createdTasks()) != 1 {
		t.Fatalf("created %d tasks from 3 deliveries, want 1", len(host.createdTasks()))
	}
}

// The activity feed is what the operator sees when something goes wrong, so a
// failure has to reach it rather than only the log.
func TestFailureIsRecordedOnTheStatus(t *testing.T) {
	newStubSlack(t, "")
	host := newFakeHost(map[string]any{
		"app_token": "xapp-1-test", "bot_token": "xoxb-test", "utility_agent": "agent-1",
	})
	host.createErr = errors.New("workspace is full")
	cfg, _ := loadConfig(host.config)
	r := newRunner(func() pluginsdk.Host { return host })

	if err := r.Handle(context.Background(), cfg, inboundRequest{
		ChannelID: "C1", TS: "2.0", Instruction: "fix sso",
	}); err == nil {
		t.Fatal("Handle should report the create failure")
	}
	raw, ok, _ := host.GetState(context.Background(), stateScope, "", stateStatus)
	if !ok {
		t.Fatal("no status was written")
	}
	st := statusFromMap(raw)
	if !strings.Contains(st.Error, "workspace is full") {
		t.Fatalf("status error = %q, want the create failure", st.Error)
	}
	if len(st.Recent) != 1 {
		t.Fatalf("recent = %v, want the failure noted", st.Recent)
	}
	entry, _ := st.Recent[0].(map[string]any)
	if entry["error"] == nil {
		t.Fatalf("recent entry = %v, want it marked as an error", entry)
	}
}

// The agent picks from the real topology, so the prompt has to carry it.
func TestPromptCarriesTheRealTopology(t *testing.T) {
	newStubSlack(t, oneMatch)
	host := newFakeHost(sessionRawConfig())
	s := newSupervisor(func() pluginsdk.Host { return host })

	s.tick(context.Background(), true)

	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.prompts) != 1 {
		t.Fatalf("invoked the agent %d times, want 1", len(host.prompts))
	}
	for _, want := range []string{"ws-1", "Engineering", "Backlog", "kandev", "fix sso"} {
		if !strings.Contains(host.prompts[0], want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
}

// A slash command is invoked privately and may be run in a channel the bot was
// never invited to. Answering with chat.postMessage would both expose a
// private action and fail outright on membership, leaving a task created with
// no feedback at all.
func TestSlashCommandRepliesThroughResponseURL(t *testing.T) {
	slack := newStubSlack(t, "")
	// Point the host check at the stub for this test; the check itself is
	// covered directly in socket_test.go.
	restore := slackHostSuffix
	stubHost, err := url.Parse(slack.srv.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	// Hostname() drops the port, so the suffix must too.
	slackHostSuffix = stubHost.Hostname()
	t.Cleanup(func() { slackHostSuffix = restore })

	host := newFakeHost(map[string]any{
		"app_token": "xapp-1-test", "bot_token": "xoxb-test", "utility_agent": "agent-1",
	})
	cfg, _ := loadConfig(host.config)
	r := newRunner(func() pluginsdk.Host { return host })

	if err := r.Handle(context.Background(), cfg, inboundRequest{
		ChannelID:   "C1",
		UserID:      "U1",
		Instruction: "fix sso",
		ResponseURL: slack.responseURL(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(host.createdTasks()) != 1 {
		t.Fatalf("created %d tasks, want 1", len(host.createdTasks()))
	}
	replies := slack.commandReplies()
	if len(replies) != 1 {
		t.Fatalf("delivered %d command replies, want 1", len(replies))
	}
	if !strings.Contains(replies[0], "Filed it.") || !strings.Contains(replies[0], "PLAT-482") {
		t.Fatalf("reply = %q, want the agent text and the identifier", replies[0])
	}
	// Ephemeral keeps the answer with the person who asked, matching how the
	// command was invoked.
	if !strings.Contains(replies[0], `"response_type":"ephemeral"`) {
		t.Fatalf("reply = %q, want an ephemeral response", replies[0])
	}
	if n := slack.callCount("chat.postMessage"); n != 0 {
		t.Fatalf("chat.postMessage called %d times, want the command route only", n)
	}
	if n := slack.callCount("reactions.add"); n != 0 {
		t.Fatalf("reactions.add called %d times; a command has no message to react to", n)
	}
}

// A mention still answers in-thread — the response_url route must not leak
// into the path that has a real message to reply under.
func TestMentionStillRepliesInChannel(t *testing.T) {
	slack := newStubSlack(t, "")
	host := newFakeHost(map[string]any{
		"app_token": "xapp-1-test", "bot_token": "xoxb-test", "utility_agent": "agent-1",
	})
	cfg, _ := loadConfig(host.config)
	r := newRunner(func() pluginsdk.Host { return host })

	if err := r.Handle(context.Background(), cfg, inboundRequest{
		ChannelID: "C1", TS: "2.0", Instruction: "fix sso", Acknowledge: true,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(slack.postedTexts()) != 1 {
		t.Fatalf("posted %d in-channel replies, want 1", len(slack.postedTexts()))
	}
	if len(slack.commandReplies()) != 0 {
		t.Fatal("a mention must not use the command response route")
	}
}
