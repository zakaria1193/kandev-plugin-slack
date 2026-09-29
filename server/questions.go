package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sync"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// The clarification bridge.
//
// Detection: Kandev publishes no bus event when an agent asks a question
// (the clarification.* subjects only cover answers and cancellations), so the
// bridge polls Interactions().ListPending for clarification bundles. The
// durable interaction record is the source of truth anyway: polling it also
// survives a plugin restart and a dropped event.
//
// Storage: one Host state entry maps each posted pending id to the Slack
// thread it lives in. A record is removed once its question is resolved, by a
// Slack reply or anywhere else, so the entry stays small.
//
// Replies: a Slack message in a tracked thread from an allow-listed user is
// parsed into one answer per question and submitted through questionAnswerer.

const (
	stateQuestionThreads = "clarification_threads"
	stateQuestionStatus  = "clarification_status"

	// answeredReaction marks the reply that answered a question.
	answeredReaction = "white_check_mark"

	// postRetryDelay spaces out retries of a question that could not be posted
	// (channel missing, bot not invited), so a broken mapping does not post
	// an error every poll.
	postRetryDelay = time.Minute

	// maxPendingPages bounds one ListPending sweep.
	maxPendingPages = 20
	pendingPageSize = 100

	// maxSeenReplies bounds the reply dedup set.
	maxSeenReplies = 512
)

// threadRecord is where one clarification bundle was posted.
type threadRecord struct {
	PendingID   string
	Channel     string
	TS          string
	TaskID      string
	WorkspaceID string
	PostedAt    string
}

func (r threadRecord) toMap() map[string]any {
	return map[string]any{
		"channel": r.Channel, "ts": r.TS, "taskId": r.TaskID,
		"workspaceId": r.WorkspaceID, "postedAt": r.PostedAt,
	}
}

func threadRecordFromMap(pendingID string, v map[string]any) threadRecord {
	return threadRecord{
		PendingID:   pendingID,
		Channel:     stringField(v, "channel"),
		TS:          stringField(v, "ts"),
		TaskID:      stringField(v, "taskId"),
		WorkspaceID: stringField(v, "workspaceId"),
		PostedAt:    stringField(v, "postedAt"),
	}
}

// threadMessage is a Slack message posted inside a thread.
type threadMessage struct {
	Channel  string
	TS       string
	ThreadTS string
	User     string
	Text     string
}

// threadRouter is what the socket listener needs from the bridge.
type threadRouter interface {
	// OwnsThread reports whether a thread is a posted clarification.
	OwnsThread(ctx context.Context, channel, threadTS string) bool
	// HandleThreadReply answers from a reply. Replies in other threads, or
	// from users outside the allow-list, are ignored.
	HandleThreadReply(ctx context.Context, msg threadMessage)
}

// bridgeSettings is the configuration a sync ran with; replies use the same.
type bridgeSettings struct {
	botToken string
	q        *questionsConfig
}

// questionBridge posts pending clarifications and answers them from replies.
type questionBridge struct {
	host func() pluginsdk.Host
	// newAnswerer builds the answer transport; tests replace it.
	newAnswerer func(q *questionsConfig) questionAnswerer

	// opMu serialises every sync and reply, so a record is never
	// reconciled away while a reply is answering it, and the state entry's
	// read-modify-write never races.
	opMu sync.Mutex

	mu         sync.Mutex
	current    *bridgeSettings
	lastSync   time.Time
	retryAfter map[string]time.Time
	seen       map[string]bool
	seenOrder  []string
}

func newQuestionBridge(host func() pluginsdk.Host) *questionBridge {
	return &questionBridge{
		host: host,
		newAnswerer: func(q *questionsConfig) questionAnswerer {
			return newKandevAPI(q.KandevURL, q.KandevToken)
		},
		retryAfter: map[string]time.Time{},
		seen:       map[string]bool{},
	}
}

var _ threadRouter = (*questionBridge)(nil)

// Sync runs one detection pass when the poll interval has elapsed (or force
// is set). cfg must be the app-mode config; a nil q turns the bridge off.
func (b *questionBridge) Sync(ctx context.Context, cfg *config, q *questionsConfig, qErr error, force bool) {
	host := b.host()
	if host == nil {
		return
	}
	b.mu.Lock()
	if q == nil || cfg == nil {
		b.current = nil
	} else {
		b.current = &bridgeSettings{botToken: cfg.BotToken, q: q}
	}
	due := force || b.lastSync.IsZero() || (q != nil && time.Since(b.lastSync) >= q.PollInterval)
	if due {
		b.lastSync = time.Now()
	}
	b.mu.Unlock()

	if q == nil {
		// A config error means "on, but broken"; no config means "off".
		b.writeStatus(ctx, host, qErr != nil, qErr, 0, 0)
		return
	}
	if !due {
		return
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	posted, err := b.sync(ctx, host, cfg.BotToken, q)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("slack: clarification sync: %v", err)
	}
	b.writeStatus(ctx, host, true, err, posted, 0)
}

func (b *questionBridge) sync(ctx context.Context, host pluginsdk.Host, botToken string, q *questionsConfig) (int, error) {
	interactions, ok := pluginsdk.Interactions(host)
	if !ok {
		return 0, errors.New("this Kandev does not expose pending questions to plugins (needs Kandev 0.92 or later)")
	}
	pending, err := listPendingClarifications(ctx, interactions)
	if err != nil {
		return 0, fmt.Errorf("list pending questions: %w", err)
	}
	threads, err := readThreads(ctx, host)
	if err != nil {
		return 0, err
	}
	cl := newClient(botToken, "")
	names := b.workspaceNames(ctx, host, q)

	var errs []error
	posted := 0
	changed := false
	pendingIDs := make(map[string]bool, len(pending))
	for i := range pending {
		in := &pending[i]
		pendingIDs[in.ID] = true
		if _, tracked := threads[in.ID]; tracked || !b.mayPost(in.ID) {
			continue
		}
		rec, err := b.post(ctx, host, cl, q, names, in)
		if err != nil {
			b.deferPost(in.ID)
			errs = append(errs, fmt.Errorf("post question %s: %w", in.ID, err))
			continue
		}
		threads[in.ID] = rec
		posted++
		changed = true
	}
	for id, rec := range threads {
		if pendingIDs[id] {
			continue
		}
		if b.reconcile(ctx, interactions, cl, rec) {
			delete(threads, id)
			changed = true
		}
	}
	if changed {
		if err := writeThreads(ctx, host, threads); err != nil {
			errs = append(errs, err)
		}
	}
	return posted, errors.Join(errs...)
}

// listPendingClarifications pages through every pending clarification.
func listPendingClarifications(ctx context.Context, interactions pluginsdk.InteractionAccessor) ([]pluginsdk.Interaction, error) {
	var out []pluginsdk.Interaction
	page := pluginsdk.Page{Limit: pendingPageSize}
	for i := 0; i < maxPendingPages; i++ {
		items, info, err := interactions.ListPending(ctx, pluginsdk.InteractionFilter{
			Kinds: []string{pluginsdk.InteractionKindClarification},
		}, page)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.Kind == pluginsdk.InteractionKindClarification {
				out = append(out, it)
			}
		}
		if info == nil || !info.HasMore || info.NextCursor == "" {
			break
		}
		page.Cursor = info.NextCursor
	}
	return out, nil
}

// post writes one clarification bundle to its workspace's channel.
func (b *questionBridge) post(
	ctx context.Context, host pluginsdk.Host, cl *client, q *questionsConfig,
	names map[string]string, in *pluginsdk.Interaction,
) (threadRecord, error) {
	task, err := host.Tasks().Get(ctx, in.TaskID)
	if err != nil {
		// The question still deserves to be seen: post it untitled to the
		// default channel rather than not at all.
		log.Printf("slack: read task %s for question %s: %v", in.TaskID, in.ID, err)
		task = nil
	}
	workspaceID := ""
	if task != nil {
		workspaceID = task.WorkspaceID
	}
	channel := q.channelFor(workspaceID, names[workspaceID])
	if channel == "" {
		return threadRecord{}, fmt.Errorf("no Slack channel is mapped for workspace %q and no default channel is set", workspaceID)
	}
	link := ""
	if q.PublicURL != "" && in.TaskID != "" {
		link = q.PublicURL + "/t/" + in.TaskID
	}
	gotChannel, ts, err := cl.PostMessageTS(ctx, channel, "", formatQuestionPost(task, link, in))
	if err != nil {
		return threadRecord{}, err
	}
	return threadRecord{
		PendingID: in.ID, Channel: gotChannel, TS: ts, TaskID: in.TaskID,
		WorkspaceID: workspaceID, PostedAt: nowRFC3339(),
	}, nil
}

// reconcile handles a tracked question that is no longer pending: it was
// answered in Kandev's UI, cancelled or expired. It reports whether the record
// can be dropped.
func (b *questionBridge) reconcile(ctx context.Context, interactions pluginsdk.InteractionAccessor, cl *client, rec threadRecord) bool {
	in, err := interactions.Get(ctx, rec.PendingID)
	switch {
	case err != nil && grpcstatus.Code(err) == codes.NotFound:
		b.say(ctx, cl, rec, "This question no longer exists in Kandev, so there is nothing left to answer here.")
		return true
	case err != nil:
		// Transient: keep the record and look again next pass.
		return false
	case in.Status == pluginsdk.InteractionStatusPending:
		// A pagination race; still open.
		return false
	case in.Status == pluginsdk.InteractionStatusAnswered:
		b.say(ctx, cl, rec, "Answered in Kandev — nothing more to do here.")
	default:
		b.say(ctx, cl, rec, fmt.Sprintf("This question was %s in Kandev — nothing more to do here.", in.Status))
	}
	return true
}

// OwnsThread reports whether the thread is a posted clarification.
func (b *questionBridge) OwnsThread(ctx context.Context, channel, threadTS string) bool {
	if threadTS == "" || b.settings() == nil {
		return false
	}
	host := b.host()
	if host == nil {
		return false
	}
	threads, err := readThreads(ctx, host)
	if err != nil {
		return false
	}
	_, ok := findThread(threads, channel, threadTS)
	return ok
}

// leadingMention strips "@Kandev" from a reply that mentions the bot.
var leadingMention = regexp.MustCompile(`^\s*<@[A-Z0-9]+(\|[^>]*)?>[\s:,]*`)

// HandleThreadReply answers a clarification from a Slack reply.
func (b *questionBridge) HandleThreadReply(ctx context.Context, msg threadMessage) {
	settings := b.settings()
	host := b.host()
	if settings == nil || host == nil || msg.ThreadTS == "" || msg.ThreadTS == msg.TS {
		return
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()

	threads, err := readThreads(ctx, host)
	if err != nil {
		log.Printf("slack: read clarification threads: %v", err)
		return
	}
	rec, ok := findThread(threads, msg.Channel, msg.ThreadTS)
	if !ok {
		return
	}
	if !settings.q.Answerers[msg.User] {
		// Other people may discuss the question in the thread; only the
		// allow-list answers it.
		return
	}
	if !b.claimReply(msg.Channel, msg.TS) {
		return
	}
	cl := newClient(settings.botToken, "")
	done := b.answer(ctx, host, cl, settings.q, rec, msg)
	if done {
		delete(threads, rec.PendingID)
		if err := writeThreads(ctx, host, threads); err != nil {
			log.Printf("slack: persist clarification threads: %v", err)
		}
	}
}

// answer resolves one reply. It reports whether the question is finished.
func (b *questionBridge) answer(
	ctx context.Context, host pluginsdk.Host, cl *client, q *questionsConfig,
	rec threadRecord, msg threadMessage,
) bool {
	interactions, ok := pluginsdk.Interactions(host)
	if !ok {
		b.say(ctx, cl, rec, "This Kandev cannot read pending questions, so the answer was not sent.")
		return false
	}
	in, err := interactions.Get(ctx, rec.PendingID)
	switch {
	case err != nil && grpcstatus.Code(err) == codes.NotFound:
		b.say(ctx, cl, rec, "This question no longer exists in Kandev (it expired or was removed), so your answer was not sent.")
		return true
	case err != nil:
		b.say(ctx, cl, rec, "Could not read the question from Kandev ("+err.Error()+"). Reply again to retry.")
		return false
	case in.Status != pluginsdk.InteractionStatusPending:
		b.say(ctx, cl, rec, fmt.Sprintf("This question was already %s in Kandev, so your answer was not sent.", in.Status))
		return true
	}

	text := leadingMention.ReplaceAllString(msg.Text, "")
	answers, err := parseReply(text, in.Questions)
	if err != nil {
		var missing *errMissingAnswers
		if errors.As(err, &missing) {
			b.say(ctx, cl, rec, "Kandev needs every question answered: "+err.Error()+
				". Reply again with one numbered line per question.")
		} else {
			b.say(ctx, cl, rec, "Could not read that reply: "+err.Error()+".")
		}
		return false
	}

	result := b.newAnswerer(q).Answer(ctx, rec.PendingID, answers)
	switch result.Outcome {
	case answerApplied:
		if err := cl.AddReaction(ctx, msg.Channel, msg.TS, answeredReaction); err != nil {
			log.Printf("slack: answered reaction failed: %v", err)
		}
		b.say(ctx, cl, rec, "Answered — the agent resumes.")
		b.countAnswered(ctx, host)
		return true
	case answerAlreadyResolved:
		b.say(ctx, cl, rec, "Your answer was not needed: "+result.Detail+".")
		return true
	case answerGone:
		b.say(ctx, cl, rec, "This question no longer exists in Kandev, so your answer was not sent.")
		return true
	case answerRejected:
		b.say(ctx, cl, rec, "Kandev did not accept that answer: "+result.Detail+". Reply again to fix it.")
		return false
	default:
		b.say(ctx, cl, rec, "The answer did not reach Kandev: "+result.Detail+". Reply again to retry.")
		b.releaseReply(msg.Channel, msg.TS)
		return false
	}
}

// say posts into the question's thread. Failures are logged only: the state
// change it reports has already happened.
func (b *questionBridge) say(ctx context.Context, cl *client, rec threadRecord, text string) {
	if err := cl.PostMessage(ctx, rec.Channel, rec.TS, text); err != nil {
		log.Printf("slack: clarification thread reply failed: %v", err)
	}
}

func (b *questionBridge) settings() *bridgeSettings {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.current
}

func (b *questionBridge) mayPost(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	until, ok := b.retryAfter[id]
	if !ok {
		return true
	}
	if time.Now().After(until) {
		delete(b.retryAfter, id)
		return true
	}
	return false
}

func (b *questionBridge) deferPost(id string) {
	b.mu.Lock()
	b.retryAfter[id] = time.Now().Add(postRetryDelay)
	b.mu.Unlock()
}

// claimReply dedups a reply: the same message can arrive both as a message
// event and, when it mentions the bot, as an app_mention.
func (b *questionBridge) claimReply(channel, ts string) bool {
	key := channel + "/" + ts
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen[key] {
		return false
	}
	b.seen[key] = true
	b.seenOrder = append(b.seenOrder, key)
	if len(b.seenOrder) > maxSeenReplies {
		delete(b.seen, b.seenOrder[0])
		b.seenOrder = b.seenOrder[1:]
	}
	return true
}

func (b *questionBridge) releaseReply(channel, ts string) {
	b.mu.Lock()
	delete(b.seen, channel+"/"+ts)
	b.mu.Unlock()
}

// workspaceNames maps workspace id → name, only when a mapping could use
// names. A failure just means id-only matching.
func (b *questionBridge) workspaceNames(ctx context.Context, host pluginsdk.Host, q *questionsConfig) map[string]string {
	names := map[string]string{}
	if len(q.Channels) == 0 {
		return names
	}
	workspaces, _, err := host.Workspaces().List(ctx, pluginsdk.Page{})
	if err != nil {
		return names
	}
	for _, w := range workspaces {
		names[w.ID] = w.Name
	}
	return names
}

func findThread(threads map[string]threadRecord, channel, threadTS string) (threadRecord, bool) {
	for _, rec := range threads {
		if rec.TS == threadTS && rec.Channel == channel {
			return rec, true
		}
	}
	return threadRecord{}, false
}

func readThreads(ctx context.Context, host pluginsdk.Host) (map[string]threadRecord, error) {
	out := map[string]threadRecord{}
	value, found, err := host.GetState(ctx, stateScope, "", stateQuestionThreads)
	if err != nil {
		return nil, fmt.Errorf("read clarification threads: %w", err)
	}
	if !found {
		return out, nil
	}
	threads, _ := value["threads"].(map[string]any)
	for id, raw := range threads {
		if m, ok := raw.(map[string]any); ok {
			out[id] = threadRecordFromMap(id, m)
		}
	}
	return out, nil
}

func writeThreads(ctx context.Context, host pluginsdk.Host, threads map[string]threadRecord) error {
	m := make(map[string]any, len(threads))
	for id, rec := range threads {
		m[id] = rec.toMap()
	}
	if err := host.SetState(ctx, stateScope, "", stateQuestionThreads, map[string]any{"threads": m}); err != nil {
		return fmt.Errorf("persist clarification threads: %w", err)
	}
	return nil
}

// statusRefresh is how stale syncedAt may get before an otherwise unchanged
// status is rewritten, so a healthy bridge does not write state every poll.
const statusRefresh = time.Minute

// writeStatus records the bridge's health for the plugin page.
func (b *questionBridge) writeStatus(ctx context.Context, host pluginsdk.Host, enabled bool, err error, posted, answered int) {
	value, _, _ := host.GetState(ctx, stateScope, "", stateQuestionStatus)
	if value == nil {
		value = map[string]any{}
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	unchanged := value["enabled"] == enabled && stringField(value, "error") == msg && posted == 0 && answered == 0
	if unchanged && (!enabled || msg != "" || recent(stringField(value, "syncedAt"), statusRefresh)) {
		return
	}
	value["enabled"] = enabled
	value["error"] = msg
	value["posted"] = numberField(value, "posted") + float64(posted)
	value["answered"] = numberField(value, "answered") + float64(answered)
	if enabled {
		value["syncedAt"] = nowRFC3339()
	}
	if setErr := host.SetState(ctx, stateScope, "", stateQuestionStatus, value); setErr != nil {
		log.Printf("slack: persist clarification status: %v", setErr)
	}
}

func recent(rfc3339 string, window time.Duration) bool {
	t, err := time.Parse(time.RFC3339, rfc3339)
	return err == nil && time.Since(t) < window
}

func (b *questionBridge) countAnswered(ctx context.Context, host pluginsdk.Host) {
	b.writeStatus(ctx, host, true, nil, 0, 1)
}

func numberField(value map[string]any, key string) float64 {
	switch v := value[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	default:
		return 0
	}
}

// readQuestionStatus returns the persisted bridge status for the plugin page.
func readQuestionStatus(ctx context.Context, host pluginsdk.Host) map[string]any {
	value, found, err := host.GetState(ctx, stateScope, "", stateQuestionStatus)
	if err != nil || !found {
		return map[string]any{"enabled": false}
	}
	return value
}
