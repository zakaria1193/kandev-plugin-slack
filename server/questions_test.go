package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// --- fakes ---

// postedMessage is one chat.postMessage (or reactions.add) the stub saw.
type postedMessage struct {
	Channel  string
	ThreadTS string
	TS       string
	Text     string
}

func (s *stubSlack) postedMessages() []postedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]postedMessage(nil), s.postRecords...)
}

func (s *stubSlack) addedReactions() []postedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]postedMessage(nil), s.reactions...)
}

func (s *stubSlack) failPosts(code string) {
	s.mu.Lock()
	s.postError = code
	s.mu.Unlock()
}

// questionHost is the fake Host plus the two readers clarification threads
// use: pending interactions and single-task reads.
type questionHost struct {
	*fakeHost

	qmu     sync.Mutex
	pending map[string]pluginsdk.Interaction
	// resolved answers Get for ids that are no longer pending.
	resolved map[string]pluginsdk.Interaction
	tasks    map[string]pluginsdk.Task
}

func newQuestionHost(config map[string]any) *questionHost {
	return &questionHost{
		fakeHost: newFakeHost(config),
		pending:  map[string]pluginsdk.Interaction{},
		resolved: map[string]pluginsdk.Interaction{},
		tasks: map[string]pluginsdk.Task{
			"task-7": {ID: "task-7", WorkspaceID: "ws-1", Title: "Spec the <login> flow", Identifier: "PLAT-7"},
		},
	}
}

func (h *questionHost) Interactions() pluginsdk.InteractionAccessor { return questionInteractions{h} }
func (h *questionHost) Tasks() pluginsdk.TaskReader                 { return questionTasks{h: h} }

func (h *questionHost) ask(in pluginsdk.Interaction) {
	h.qmu.Lock()
	defer h.qmu.Unlock()
	in.Kind = pluginsdk.InteractionKindClarification
	in.Status = pluginsdk.InteractionStatusPending
	h.pending[in.ID] = in
}

// resolve moves a question out of the pending list, as answering it in the
// Kandev UI would.
func (h *questionHost) resolve(id, status string) {
	h.qmu.Lock()
	defer h.qmu.Unlock()
	in := h.pending[id]
	delete(h.pending, id)
	in.Status = status
	h.resolved[id] = in
}

func (h *questionHost) forget(id string) {
	h.qmu.Lock()
	defer h.qmu.Unlock()
	delete(h.pending, id)
	delete(h.resolved, id)
}

func (h *questionHost) threads(t *testing.T) map[string]threadRecord {
	t.Helper()
	threads, err := readThreads(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	return threads
}

type questionInteractions struct{ h *questionHost }

func (q questionInteractions) ListPending(_ context.Context, filter pluginsdk.InteractionFilter, _ pluginsdk.Page) ([]pluginsdk.Interaction, *pluginsdk.PageInfo, error) {
	if len(filter.Kinds) != 1 || filter.Kinds[0] != pluginsdk.InteractionKindClarification {
		return nil, nil, errors.New("bridge must ask for clarifications only")
	}
	q.h.qmu.Lock()
	defer q.h.qmu.Unlock()
	out := make([]pluginsdk.Interaction, 0, len(q.h.pending))
	for _, in := range q.h.pending {
		out = append(out, in)
	}
	return out, &pluginsdk.PageInfo{}, nil
}

func (q questionInteractions) Get(_ context.Context, id string) (*pluginsdk.Interaction, error) {
	q.h.qmu.Lock()
	defer q.h.qmu.Unlock()
	if in, ok := q.h.pending[id]; ok {
		return &in, nil
	}
	if in, ok := q.h.resolved[id]; ok {
		return &in, nil
	}
	return nil, grpcstatus.Errorf(codes.NotFound, "interaction %q not found", id)
}

func (questionInteractions) RespondToPermission(context.Context, pluginsdk.PermissionResponse) (*pluginsdk.Interaction, error) {
	return nil, errors.New("not used")
}

func (questionInteractions) AnswerClarification(context.Context, pluginsdk.ClarificationResponse) (*pluginsdk.Interaction, error) {
	return nil, errors.New("the v1 answer path is denied by Kandev; the bridge must not use it")
}

func (questionInteractions) CancelClarification(context.Context, string, string) (*pluginsdk.Interaction, error) {
	return nil, errors.New("not used")
}

type questionTasks struct {
	pluginsdk.TaskReader
	h *questionHost
}

func (q questionTasks) Get(_ context.Context, id string) (*pluginsdk.Task, error) {
	q.h.qmu.Lock()
	defer q.h.qmu.Unlock()
	task, ok := q.h.tasks[id]
	if !ok {
		return nil, grpcstatus.Errorf(codes.NotFound, "task %q not found", id)
	}
	return &task, nil
}

// fakeAnswerer records what the bridge submitted and returns a set outcome.
type fakeAnswerer struct {
	mu      sync.Mutex
	calls   []answerCall
	results []answerResult
	host    *questionHost
}

type answerCall struct {
	PendingID string
	Answers   []pluginsdk.ClarificationAnswer
}

func (f *fakeAnswerer) Answer(_ context.Context, id string, answers []pluginsdk.ClarificationAnswer) answerResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, answerCall{PendingID: id, Answers: answers})
	result := answerResult{Outcome: answerApplied}
	if len(f.results) > 0 {
		result = f.results[0]
		f.results = f.results[1:]
	}
	if result.Outcome == answerApplied && f.host != nil {
		f.host.resolve(id, pluginsdk.InteractionStatusAnswered)
	}
	return result
}

func (f *fakeAnswerer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func questionsRawConfig() map[string]any {
	return map[string]any{
		"app_token":                 "xapp-test",
		"bot_token":                 "xoxb-test",
		"utility_agent":             "agent-1",
		"questions_enabled":         true,
		"kandev_url":                "http://127.0.0.1:3040",
		"kandev_public_url":         "https://kandev.example.com",
		"questions_default_channel": "CDEFAULT",
		"questions_answerers":       "UOWNER",
	}
}

// questionRig is a bridge wired to a stub Slack, a fake Host and a fake
// Kandev answer route.
type questionRig struct {
	slack    *stubSlack
	host     *questionHost
	answerer *fakeAnswerer
	bridge   *questionBridge
	cfg      *config
	replies  int
}

func newQuestionRig(t *testing.T, raw map[string]any) *questionRig {
	t.Helper()
	slack := newStubSlack(t, `{"ok":true}`)
	host := newQuestionHost(raw)
	cfg, err := loadConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuestionsErr != nil {
		t.Fatal(cfg.QuestionsErr)
	}
	answerer := &fakeAnswerer{host: host}
	bridge := newQuestionBridge(func() pluginsdk.Host { return host })
	bridge.newAnswerer = func(*questionsConfig) questionAnswerer { return answerer }
	return &questionRig{slack: slack, host: host, answerer: answerer, bridge: bridge, cfg: cfg}
}

func (r *questionRig) sync() {
	r.bridge.Sync(context.Background(), r.cfg, r.cfg.Questions, nil, true)
}

// reply delivers a new thread reply; each call is a distinct Slack message.
func (r *questionRig) reply(user, text string, threadTS string) {
	r.replies++
	r.bridge.HandleThreadReply(context.Background(), threadMessage{
		Channel: "CDEFAULT", TS: fmt.Sprintf("200.%06d", r.replies), ThreadTS: threadTS, User: user, Text: text,
	})
}

func twoQuestions() pluginsdk.Interaction {
	return pluginsdk.Interaction{
		ID: "pending-1", TaskID: "task-7", Context: "Planning the spec",
		Questions: []pluginsdk.InteractionQuestion{
			{ID: "q-db", Prompt: "Which database?", Options: []pluginsdk.InteractionOption{
				{OptionID: "opt-pg", Label: "Postgres"}, {OptionID: "opt-sqlite", Label: "SQLite", Description: "embedded"},
			}},
			{ID: "q-scope", Prompt: "Anything out of scope?"},
		},
	}
}

// --- reply parsing ---

func TestParseReplySingleQuestionTakesTheWholeText(t *testing.T) {
	answers, err := parseReply("Use the existing\nlogin page &amp; keep SSO", []pluginsdk.InteractionQuestion{{ID: "q1", Prompt: "?"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || answers[0].QuestionID != "q1" || answers[0].CustomText != "Use the existing\nlogin page & keep SSO" {
		t.Fatalf("answers = %+v", answers)
	}
}

func TestParseReplyNumberedLinesAnswerEachQuestion(t *testing.T) {
	in := twoQuestions()
	answers, err := parseReply("1. b\n2) no migrations\nand no UI work", in.Questions)
	if err != nil {
		t.Fatal(err)
	}
	if answers[0].QuestionID != "q-db" || len(answers[0].SelectedOptions) != 1 || answers[0].SelectedOptions[0] != "opt-sqlite" {
		t.Fatalf("answer 1 = %+v, want option b (opt-sqlite)", answers[0])
	}
	if answers[1].QuestionID != "q-scope" || answers[1].CustomText != "no migrations\nand no UI work" {
		t.Fatalf("answer 2 = %+v", answers[1])
	}
}

func TestParseReplyUnnumberedTextAnswersEveryQuestion(t *testing.T) {
	in := twoQuestions()
	answers, err := parseReply("you decide", in.Questions)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range answers {
		if a.CustomText != "you decide" || a.QuestionID != in.Questions[i].ID {
			t.Fatalf("answer %d = %+v", i, a)
		}
	}
}

func TestParseReplyNumberedButIncompleteIsRejected(t *testing.T) {
	_, err := parseReply("2. nothing", twoQuestions().Questions)
	var missing *errMissingAnswers
	if !errors.As(err, &missing) || len(missing.Missing) != 1 || missing.Missing[0] != 1 {
		t.Fatalf("err = %v, want question 1 reported missing", err)
	}
}

func TestParseReplyMatchesOptionsByLabelAndID(t *testing.T) {
	q := twoQuestions().Questions[0]
	for text, want := range map[string]string{"postgres": "opt-pg", "*SQLite*": "opt-sqlite", "opt-pg": "opt-pg", "A)": "opt-pg"} {
		a := answerFor(q, text)
		if len(a.SelectedOptions) != 1 || a.SelectedOptions[0] != want {
			t.Fatalf("answerFor(%q) = %+v, want %s", text, a, want)
		}
	}
	if a := answerFor(q, "MySQL please"); len(a.SelectedOptions) != 0 || a.CustomText != "MySQL please" {
		t.Fatalf("free text became %+v", a)
	}
	// A letter past the options is text, not a choice.
	if a := answerFor(q, "z"); a.CustomText != "z" {
		t.Fatalf("answerFor(z) = %+v", a)
	}
}

func TestParseReplyRejectsEmptyText(t *testing.T) {
	if _, err := parseReply("   ", twoQuestions().Questions); err == nil {
		t.Fatal("an empty reply must not answer")
	}
}

// --- formatting ---

func TestFormatQuestionPost(t *testing.T) {
	in := twoQuestions()
	task := &pluginsdk.Task{ID: "task-7", Title: "Spec the <login> flow", Identifier: "PLAT-7"}
	text := formatQuestionPost(task, "https://kandev.example.com/t/task-7", &in)
	for _, want := range []string{
		"<https://kandev.example.com/t/task-7|Spec the &lt;login&gt; flow> (PLAT-7)",
		"> Planning the spec",
		"*1.* Which database?",
		"a) Postgres",
		"b) SQLite — _embedded_",
		"*2.* Anything out of scope?",
		"`1. …`",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("post is missing %q:\n%s", want, text)
		}
	}
}

func TestFormatQuestionPostSingleQuestionHasNoNumbers(t *testing.T) {
	in := pluginsdk.Interaction{Questions: []pluginsdk.InteractionQuestion{{ID: "q", Prompt: "Ship it?"}}}
	text := formatQuestionPost(nil, "", &in)
	if strings.Contains(text, "*1.*") || strings.Contains(text, "`1. …`") || !strings.Contains(text, "*a Kandev task*") {
		t.Fatalf("unexpected post:\n%s", text)
	}
}

// --- configuration ---

func TestQuestionsConfigOffByDefault(t *testing.T) {
	cfg, err := loadConfig(map[string]any{"app_token": "xapp-1", "bot_token": "xoxb-1", "utility_agent": "a"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Questions != nil || cfg.QuestionsErr != nil {
		t.Fatalf("questions = %+v, err = %v; want off", cfg.Questions, cfg.QuestionsErr)
	}
}

func TestQuestionsConfigErrorsDoNotBlockTriage(t *testing.T) {
	cases := map[string]func(map[string]any){
		"no url":       func(m map[string]any) { delete(m, "kandev_url") },
		"bad url":      func(m map[string]any) { m["kandev_url"] = "localhost:3040" },
		"no channel":   func(m map[string]any) { delete(m, "questions_default_channel") },
		"no answerers": func(m map[string]any) { m["questions_answerers"] = " , " },
		"bad mapping":  func(m map[string]any) { m["questions_channels"] = "Homelab" },
	}
	for name, mutate := range cases {
		raw := questionsRawConfig()
		mutate(raw)
		cfg, err := loadConfig(raw)
		if err != nil {
			t.Fatalf("%s: triage config failed too: %v", name, err)
		}
		if cfg.Questions != nil || cfg.QuestionsErr == nil {
			t.Fatalf("%s: want a questions error, got %+v", name, cfg.Questions)
		}
	}
}

func TestQuestionsNeedTheSlackApp(t *testing.T) {
	raw := sessionRawConfig()
	for k, v := range questionsRawConfig() {
		if strings.HasPrefix(k, "questions_") || strings.HasPrefix(k, "kandev_") {
			raw[k] = v
		}
	}
	cfg, err := loadConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuestionsErr == nil || !strings.Contains(cfg.QuestionsErr.Error(), "Socket Mode") {
		t.Fatalf("err = %v, want the Slack app requirement", cfg.QuestionsErr)
	}
}

func TestQuestionsChannelMapping(t *testing.T) {
	raw := questionsRawConfig()
	raw["questions_channels"] = "ws-2=C222\nHome Lab = #C333"
	raw["questions_answerers"] = "UOWNER, <@UOTHER>"
	raw["questions_poll_seconds"] = float64(1)
	cfg, err := loadConfig(raw)
	if err != nil || cfg.QuestionsErr != nil {
		t.Fatal(err, cfg.QuestionsErr)
	}
	q := cfg.Questions
	if got := q.channelFor("WS-2", ""); got != "C222" {
		t.Fatalf("by id = %q", got)
	}
	if got := q.channelFor("ws-9", "home lab"); got != "C333" {
		t.Fatalf("by name = %q", got)
	}
	if got := q.channelFor("ws-9", "Other"); got != "CDEFAULT" {
		t.Fatalf("fallback = %q", got)
	}
	if !q.Answerers["UOWNER"] || !q.Answerers["UOTHER"] {
		t.Fatalf("answerers = %v", q.Answerers)
	}
	if q.PollInterval.Seconds() != minQuestionPollSeconds {
		t.Fatalf("poll interval = %v, want clamped to the minimum", q.PollInterval)
	}
}

// --- Kandev answer route ---

func TestKandevAPIAnswerPostsTheBundle(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"success":true,"claimed":true,"status":"answered"}`))
	}))
	defer srv.Close()

	res := newKandevAPI(srv.URL+"/", "kandev_pat_abc_def").Answer(context.Background(), "pend/1", []pluginsdk.ClarificationAnswer{
		{QuestionID: "q1", SelectedOptions: []string{"o1"}}, {QuestionID: "q2", CustomText: "free"},
	})
	if res.Outcome != answerApplied {
		t.Fatalf("outcome = %+v", res)
	}
	if gotPath != "/api/v1/clarification/pend%2F1/respond" && gotPath != "/api/v1/clarification/pend/1/respond" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer kandev_pat_abc_def" {
		t.Fatalf("auth = %q", gotAuth)
	}
	answers, _ := gotBody["answers"].([]any)
	if len(answers) != 2 {
		t.Fatalf("body = %v", gotBody)
	}
	first, _ := answers[0].(map[string]any)
	if first["question_id"] != "q1" || first["custom_text"] != nil {
		t.Fatalf("first answer = %v", first)
	}
}

func TestKandevAPIOmitsAuthWithoutAToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Authorization header")
		}
		_, _ = w.Write([]byte(`{"claimed":true}`))
	}))
	defer srv.Close()
	if res := newKandevAPI(srv.URL, "").Answer(context.Background(), "p", nil); res.Outcome != answerApplied {
		t.Fatalf("outcome = %+v", res)
	}
}

func TestKandevAPIUnreachableIsRetryable(t *testing.T) {
	res := newKandevAPI("http://127.0.0.1:1", "").Answer(context.Background(), "p", nil)
	if res.Outcome != answerFailed || !strings.Contains(res.Detail, "cannot reach Kandev") {
		t.Fatalf("outcome = %+v", res)
	}
}

func TestClassifyRespond(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   answerOutcome
	}{
		{200, `{"claimed":true,"status":"answered"}`, answerApplied},
		{200, `{"claimed":false,"status":"answered"}`, answerAlreadyResolved},
		{404, `{"error":"clarification request not found"}`, answerGone},
		{409, `{"error":"clarification request is no longer active"}`, answerAlreadyResolved},
		{400, `{"error":"expected exactly 2 answer(s), got 1"}`, answerRejected},
		{401, `{"error":"unauthorized"}`, answerFailed},
		{503, `{"error":"busy"}`, answerFailed},
		{500, `oops`, answerFailed},
	}
	for _, c := range cases {
		if got := classifyRespond(c.status, []byte(c.body)); got.Outcome != c.want {
			t.Fatalf("classifyRespond(%d, %s) = %+v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// --- the bridge end to end ---

func TestBridgePostsAPendingQuestionOnce(t *testing.T) {
	rig := newQuestionRig(t, questionsRawConfig())
	rig.host.ask(twoQuestions())

	rig.sync()
	rig.sync()

	posts := rig.slack.postedMessages()
	if len(posts) != 1 {
		t.Fatalf("posted %d messages, want 1", len(posts))
	}
	if posts[0].Channel != "CDEFAULT" || posts[0].ThreadTS != "" || !strings.Contains(posts[0].Text, "Which database?") {
		t.Fatalf("post = %+v", posts[0])
	}
	if !strings.Contains(posts[0].Text, "https://kandev.example.com/t/task-7") {
		t.Fatalf("post has no task link:\n%s", posts[0].Text)
	}
	rec, ok := rig.host.threads(t)["pending-1"]
	if !ok || rec.Channel != "CDEFAULT" || rec.TS != posts[0].TS || rec.TaskID != "task-7" || rec.WorkspaceID != "ws-1" {
		t.Fatalf("thread record = %+v", rec)
	}
	status := readQuestionStatus(context.Background(), rig.host)
	if status["enabled"] != true || status["posted"] != float64(1) || status["error"] != "" {
		t.Fatalf("status = %v", status)
	}
}

func TestBridgeRoutesByWorkspaceName(t *testing.T) {
	raw := questionsRawConfig()
	raw["questions_channels"] = "Platform=CPLATFORM"
	rig := newQuestionRig(t, raw)
	rig.host.ask(twoQuestions())
	rig.sync()
	if posts := rig.slack.postedMessages(); len(posts) != 1 || posts[0].Channel != "CPLATFORM" {
		t.Fatalf("posts = %+v, want the Platform workspace's channel", posts)
	}
}

func TestBridgeReplyAnswersAndResumes(t *testing.T) {
	rig := newQuestionRig(t, questionsRawConfig())
	rig.host.ask(twoQuestions())
	rig.sync()
	parent := rig.slack.postedMessages()[0].TS

	rig.bridge.HandleThreadReply(context.Background(), threadMessage{
		Channel: "CDEFAULT", TS: "300.1", ThreadTS: parent, User: "UOWNER",
		Text: "<@U0BOT> 1. postgres\n2. billing",
	})

	if rig.answerer.callCount() != 1 {
		t.Fatalf("answered %d times, want 1", rig.answerer.callCount())
	}
	call := rig.answerer.calls[0]
	if call.PendingID != "pending-1" || call.Answers[0].SelectedOptions[0] != "opt-pg" || call.Answers[1].CustomText != "billing" {
		t.Fatalf("answer call = %+v", call)
	}
	reactions := rig.slack.addedReactions()
	if len(reactions) != 1 || reactions[0].TS != "300.1" || reactions[0].Text != answeredReaction {
		t.Fatalf("reactions = %+v, want ✅ on the reply", reactions)
	}
	posts := rig.slack.postedMessages()
	last := posts[len(posts)-1]
	if last.ThreadTS != parent || last.Text != "Answered — the agent resumes." {
		t.Fatalf("confirmation = %+v", last)
	}
	if _, ok := rig.host.threads(t)["pending-1"]; ok {
		t.Fatal("an answered question must stop being tracked")
	}
	// The same reply delivered again (message event + app_mention) is a no-op.
	rig.bridge.HandleThreadReply(context.Background(), threadMessage{
		Channel: "CDEFAULT", TS: "300.1", ThreadTS: parent, User: "UOWNER", Text: "1. postgres\n2. billing",
	})
	if rig.answerer.callCount() != 1 {
		t.Fatal("a duplicate delivery answered twice")
	}
	// Answering in Slack must not produce a second "answered in Kandev" note.
	before := len(rig.slack.postedMessages())
	rig.sync()
	if len(rig.slack.postedMessages()) != before {
		t.Fatal("sync posted again after a Slack answer")
	}
}

func TestBridgeIgnoresUsersOutsideTheAllowList(t *testing.T) {
	rig := newQuestionRig(t, questionsRawConfig())
	rig.host.ask(twoQuestions())
	rig.sync()
	parent := rig.slack.postedMessages()[0].TS

	rig.reply("USTRANGER", "1. a\n2. b", parent)

	if rig.answerer.callCount() != 0 || len(rig.slack.postedMessages()) != 1 {
		t.Fatal("a reply from a user outside the allow-list must be ignored")
	}
	if _, ok := rig.host.threads(t)["pending-1"]; !ok {
		t.Fatal("the question must stay tracked")
	}
}

func TestBridgeIgnoresOtherThreads(t *testing.T) {
	rig := newQuestionRig(t, questionsRawConfig())
	rig.host.ask(twoQuestions())
	rig.sync()
	rig.reply("UOWNER", "1. a\n2. b", "999.000001")
	if rig.answerer.callCount() != 0 {
		t.Fatal("a reply in an unrelated thread answered a question")
	}
	if rig.bridge.OwnsThread(context.Background(), "CDEFAULT", "999.000001") {
		t.Fatal("OwnsThread claimed an unrelated thread")
	}
	if !rig.bridge.OwnsThread(context.Background(), "CDEFAULT", rig.slack.postedMessages()[0].TS) {
		t.Fatal("OwnsThread missed the question thread")
	}
}

func TestBridgeAlreadyAnsweredElsewhere(t *testing.T) {
	rig := newQuestionRig(t, questionsRawConfig())
	rig.host.ask(twoQuestions())
	rig.sync()
	parent := rig.slack.postedMessages()[0].TS
	rig.host.resolve("pending-1", pluginsdk.InteractionStatusAnswered)

	rig.reply("UOWNER", "1. a\n2. b", parent)

	if rig.answerer.callCount() != 0 {
		t.Fatal("an already answered question must not be answered again")
	}
	last := rig.slack.postedMessages()[len(rig.slack.postedMessages())-1]
	if !strings.Contains(last.Text, "already answered") {
		t.Fatalf("reply = %q", last.Text)
	}
	if len(rig.host.threads(t)) != 0 {
		t.Fatal("the record should be dropped")
	}
}

func TestBridgeExpiredQuestion(t *testing.T) {
	rig := newQuestionRig(t, questionsRawConfig())
	rig.host.ask(twoQuestions())
	rig.sync()
	parent := rig.slack.postedMessages()[0].TS
	rig.host.forget("pending-1")

	rig.reply("UOWNER", "anything", parent)

	last := rig.slack.postedMessages()[len(rig.slack.postedMessages())-1]
	if rig.answerer.callCount() != 0 || !strings.Contains(last.Text, "no longer exists") {
		t.Fatalf("calls = %d, reply = %q", rig.answerer.callCount(), last.Text)
	}
}

func TestBridgeLostRaceIsReported(t *testing.T) {
	rig := newQuestionRig(t, questionsRawConfig())
	rig.host.ask(twoQuestions())
	rig.sync()
	parent := rig.slack.postedMessages()[0].TS
	rig.answerer.results = []answerResult{{Outcome: answerAlreadyResolved, Detail: "it was already answered"}}

	rig.reply("UOWNER", "sure", parent)

	last := rig.slack.postedMessages()[len(rig.slack.postedMessages())-1]
	if !strings.Contains(last.Text, "not needed") || len(rig.slack.addedReactions()) != 0 {
		t.Fatalf("reply = %q, reactions = %v", last.Text, rig.slack.addedReactions())
	}
	if len(rig.host.threads(t)) != 0 {
		t.Fatal("the record should be dropped")
	}
}

func TestBridgeIncompleteNumberedReplyAsksAgain(t *testing.T) {
	rig := newQuestionRig(t, questionsRawConfig())
	rig.host.ask(twoQuestions())
	rig.sync()
	parent := rig.slack.postedMessages()[0].TS

	rig.reply("UOWNER", "1. postgres", parent)

	if rig.answerer.callCount() != 0 {
		t.Fatal("an incomplete reply must not be submitted")
	}
	last := rig.slack.postedMessages()[len(rig.slack.postedMessages())-1]
	if !strings.Contains(last.Text, "no answer for question 2") {
		t.Fatalf("reply = %q", last.Text)
	}
	rig.reply("UOWNER", "1. postgres\n2. nothing", parent)
	if rig.answerer.callCount() != 1 {
		t.Fatal("a corrected reply should answer")
	}
}

func TestBridgeRetriesWhenKandevIsDown(t *testing.T) {
	rig := newQuestionRig(t, questionsRawConfig())
	rig.host.ask(twoQuestions())
	rig.sync()
	parent := rig.slack.postedMessages()[0].TS
	rig.answerer.results = []answerResult{{Outcome: answerFailed, Detail: "cannot reach Kandev"}}

	msg := threadMessage{Channel: "CDEFAULT", TS: "400.1", ThreadTS: parent, User: "UOWNER", Text: "sure"}
	rig.bridge.HandleThreadReply(context.Background(), msg)
	last := rig.slack.postedMessages()[len(rig.slack.postedMessages())-1]
	if !strings.Contains(last.Text, "did not reach Kandev") {
		t.Fatalf("reply = %q", last.Text)
	}
	if _, ok := rig.host.threads(t)["pending-1"]; !ok {
		t.Fatal("a failed delivery must keep the question tracked")
	}
	// The same reply can be retried once Kandev is back.
	rig.bridge.HandleThreadReply(context.Background(), msg)
	if rig.answerer.callCount() != 2 {
		t.Fatalf("answer calls = %d, want the retry to go through", rig.answerer.callCount())
	}
}

func TestBridgeReconcilesQuestionsAnsweredInKandev(t *testing.T) {
	rig := newQuestionRig(t, questionsRawConfig())
	rig.host.ask(twoQuestions())
	rig.host.ask(pluginsdk.Interaction{ID: "pending-2", TaskID: "task-7", Questions: []pluginsdk.InteractionQuestion{{ID: "q", Prompt: "Go?"}}})
	rig.sync()
	if len(rig.slack.postedMessages()) != 2 {
		t.Fatalf("posted %d, want 2", len(rig.slack.postedMessages()))
	}

	rig.host.resolve("pending-1", pluginsdk.InteractionStatusAnswered)
	rig.host.resolve("pending-2", pluginsdk.InteractionStatusCancelled)
	rig.sync()

	notes := map[string]bool{}
	for _, p := range rig.slack.postedMessages()[2:] {
		notes[p.Text] = p.ThreadTS != ""
	}
	if !notes["Answered in Kandev — nothing more to do here."] || !notes["This question was cancelled in Kandev — nothing more to do here."] {
		t.Fatalf("notes = %v", notes)
	}
	if len(rig.host.threads(t)) != 0 {
		t.Fatal("resolved questions must stop being tracked")
	}
}

func TestBridgeDefersAFailedPost(t *testing.T) {
	rig := newQuestionRig(t, questionsRawConfig())
	rig.slack.failPosts("not_in_channel")
	rig.host.ask(twoQuestions())

	rig.sync()
	status := readQuestionStatus(context.Background(), rig.host)
	if !strings.Contains(stringField(status, "error"), "not_in_channel") {
		t.Fatalf("status = %v, want the Slack error surfaced", status)
	}
	rig.slack.failPosts("")
	rig.sync()
	if len(rig.slack.postedMessages()) != 0 {
		t.Fatal("a failed post must wait for its retry delay")
	}
	if len(rig.host.threads(t)) != 0 {
		t.Fatal("nothing should be tracked for an unposted question")
	}
}

func TestBridgeOffDoesNothing(t *testing.T) {
	raw := questionsRawConfig()
	raw["questions_enabled"] = false
	rig := newQuestionRig(t, raw)
	rig.host.ask(twoQuestions())
	rig.sync()
	if len(rig.slack.postedMessages()) != 0 {
		t.Fatal("posted with clarification threads off")
	}
	if rig.bridge.OwnsThread(context.Background(), "CDEFAULT", "100.000001") {
		t.Fatal("owns threads while off")
	}
}

// The supervisor runs the bridge on its normal tick in Socket Mode.
func TestSupervisorTickRunsTheBridge(t *testing.T) {
	slack := newStubSlack(t, `{"ok":true}`)
	host := newQuestionHost(questionsRawConfig())
	host.ask(twoQuestions())
	s := newSupervisor(func() pluginsdk.Host { return host })
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		s.stopSource()
	}()

	s.tick(ctx, true)

	found := false
	for _, p := range slack.postedMessages() {
		if strings.Contains(p.Text, "Which database?") {
			found = true
		}
	}
	if !found {
		t.Fatalf("posts = %+v, want the question posted", slack.postedMessages())
	}
}
