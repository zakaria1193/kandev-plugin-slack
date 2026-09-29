package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// probeInterval is how often stored credentials are re-validated in the
// session fallback. Socket Mode needs no probe of its own: the connection
// state is the health signal.
const probeInterval = 90 * time.Second

// baseTick is how often the supervisor re-reads config and, in fallback mode,
// checks whether a poll is due.
const baseTick = 5 * time.Second

// supervisor owns whichever source the current configuration selects, and
// swaps it when the configuration changes. A config update restarts the plugin
// subprocess, so in practice this starts once — but a restart is not
// guaranteed for every path, and a source that outlived its credentials would
// keep talking to Slack with them.
type supervisor struct {
	host      func() pluginsdk.Host
	runner    *runner
	questions *questionBridge

	scanNow chan struct{}

	mu sync.Mutex
	// active fingerprints the config the current source was started for.
	active    string
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	lastScan  time.Time
	lastProbe time.Time
	// probedFor fingerprints the credentials the last auth probe validated. It
	// is deliberately separate from `active`, which the source lifecycle
	// clears on every tick in fallback mode — sharing them made the fallback
	// re-probe on every single scan instead of once per probeInterval.
	probedFor  string
	socketUp   bool
	socketErr  string
	socketSeen bool
}

func newSupervisor(host func() pluginsdk.Host) *supervisor {
	return &supervisor{
		host:      host,
		runner:    newRunner(host),
		questions: newQuestionBridge(host),
		scanNow:   make(chan struct{}, 1),
	}
}

// Run drives the supervisor until ctx is cancelled.
func (s *supervisor) Run(ctx context.Context) {
	ticker := time.NewTicker(baseTick)
	defer ticker.Stop()
	defer s.stopSource()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.scanNow:
			s.tick(ctx, true)
		case <-ticker.C:
			s.tick(ctx, false)
		}
	}
}

// ScanNow requests an immediate pass. Non-blocking: a pending request already
// covers the caller's intent.
func (s *supervisor) ScanNow() {
	select {
	case s.scanNow <- struct{}{}:
	default:
	}
}

func (s *supervisor) tick(ctx context.Context, force bool) {
	host := s.host()
	if host == nil {
		return
	}
	raw, err := host.GetConfig(ctx)
	if err != nil {
		log.Printf("slack: read config: %v", err)
		return
	}
	cfg, err := loadConfig(raw)
	if err != nil {
		s.stopSource()
		s.recordUnconfigured(ctx, host, err)
		return
	}
	if cfg.Mode == authModeApp {
		s.ensureSocket(ctx, cfg)
		s.publishSocketStatus(ctx, host, cfg)
		s.questions.Sync(ctx, cfg, cfg.Questions, cfg.QuestionsErr, force)
		return
	}
	s.stopSource()
	s.questions.Sync(ctx, nil, nil, cfg.QuestionsErr, force)
	s.pollOnce(ctx, host, cfg, force)
}

// ensureSocket starts the Socket Mode listener, restarting it when the
// credentials change.
func (s *supervisor) ensureSocket(ctx context.Context, cfg *config) {
	fingerprint := credentialFingerprint(cfg)
	s.mu.Lock()
	if s.active == fingerprint && s.cancel != nil {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	s.stopSource()

	// The bot's own user id is needed to strip `<@BOT>` from a mention. A
	// probe failure is not fatal: without the id the mention text keeps its
	// prefix, which the agent tolerates, and the connection error surfaces on
	// the plugin page anyway.
	botUserID := ""
	if res, err := newClient(cfg.BotToken, "").AuthTest(ctx); err == nil && res.OK {
		botUserID = res.UserID
	}

	sourceCtx, cancel := context.WithCancel(ctx)
	listener := &socketListener{
		appToken:  cfg.AppToken,
		botUserID: botUserID,
		handle: func(reqCtx context.Context, req inboundRequest) {
			// Already acknowledged to Slack; the error is recorded on the
			// status record by Handle itself.
			_ = s.runner.Handle(reqCtx, cfg, req)
		},
		onState:   s.noteSocketState,
		questions: s.questions,
	}
	s.mu.Lock()
	s.active = fingerprint
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		listener.Run(sourceCtx)
	}()
}

func (s *supervisor) noteSocketState(connected bool, err error) {
	s.mu.Lock()
	s.socketUp = connected
	s.socketSeen = true
	if err != nil {
		s.socketErr = err.Error()
	} else if connected {
		s.socketErr = ""
	}
	s.mu.Unlock()
}

func (s *supervisor) stopSource() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.active = ""
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		s.wg.Wait()
	}
}

// publishSocketStatus mirrors the live connection state into Host state so the
// plugin page shows whether events are actually arriving.
func (s *supervisor) publishSocketStatus(ctx context.Context, host pluginsdk.Host, cfg *config) {
	s.mu.Lock()
	up, seen, socketErr := s.socketUp, s.socketSeen, s.socketErr
	s.mu.Unlock()
	if !seen {
		return
	}
	st := readStatus(ctx, host)
	if st.Configured && st.OK == up && st.Error == socketErr && st.Mode == cfg.Mode.String() {
		return
	}
	st.Configured = true
	st.Mode = cfg.Mode.String()
	st.ModeLabel = cfg.Mode.Label()
	st.OK = up
	st.Error = socketErr
	st.CheckedAt = nowRFC3339()
	if err := writeStatus(ctx, host, st); err != nil {
		log.Printf("slack: persist status: %v", err)
	}
}

// recordUnconfigured persists the reason the plugin is idle so the operator
// sees it on the plugin page rather than only in the backend log.
func (s *supervisor) recordUnconfigured(ctx context.Context, host pluginsdk.Host, cause error) {
	message := ""
	if !errors.Is(cause, errNotConfigured) {
		message = cause.Error()
	}
	st := readStatus(ctx, host)
	if !st.Configured && st.Error == message {
		return
	}
	st.Configured = false
	st.OK = false
	st.Error = message
	st.Mode = ""
	st.ModeLabel = ""
	st.CheckedAt = nowRFC3339()
	if err := writeStatus(ctx, host, st); err != nil {
		log.Printf("slack: persist status: %v", err)
	}
}

// --- session fallback: polling ---

// pollOnce runs the fallback scan when its cadence has elapsed.
func (s *supervisor) pollOnce(ctx context.Context, host pluginsdk.Host, cfg *config, force bool) {
	if !force && !s.due(cfg.PollInterval) {
		return
	}
	s.markScanned()
	if err := s.scan(ctx, host, cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("slack: scan failed: %v", err)
	}
}

func (s *supervisor) due(interval time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastScan.IsZero() || time.Since(s.lastScan) >= interval
}

func (s *supervisor) markScanned() {
	s.mu.Lock()
	s.lastScan = time.Now()
	s.mu.Unlock()
}

// scan validates credentials if due, collects fresh matches, triages each,
// then advances the watermark.
func (s *supervisor) scan(ctx context.Context, host pluginsdk.Host, cfg *config) error {
	token, cookie := cfg.WebCredentials()
	cl := newClient(token, cookie)
	st := readStatus(ctx, host)
	st.Configured = true
	st.Mode = cfg.Mode.String()
	st.ModeLabel = cfg.Mode.Label()

	if err := s.ensureProbed(ctx, cl, cfg, &st); err != nil {
		return err
	}
	if !st.OK {
		st.ScannedAt = nowRFC3339()
		return writeStatus(ctx, host, st)
	}

	marks := readWatermarks(ctx, host)
	matches, err := collect(ctx, cl, cfg, st.UserID, marks)
	if err != nil {
		st.Error = err.Error()
		st.ScannedAt = nowRFC3339()
		_ = writeStatus(ctx, host, st)
		return err
	}
	st.ScannedAt = nowRFC3339()
	if err := writeStatus(ctx, host, st); err != nil {
		return err
	}
	advanced := false
	for _, m := range matches {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := s.runner.Handle(ctx, cfg, inboundRequest{
			ChannelID:   m.ChannelID,
			TS:          m.TS,
			ThreadTS:    m.ThreadTS,
			UserID:      m.UserID,
			UserName:    m.UserName,
			Text:        m.Text,
			Instruction: stripCommandPrefix(m.Text, cfg.CommandPrefix),
			Permalink:   m.Permalink,
			Acknowledge: true,
		})
		if err != nil {
			// Stop the batch here rather than skipping past the failure: the
			// watermark is a single high-water mark, so advancing over a
			// message that never became a task would drop the request
			// permanently. The next scan retries from this point.
			break
		}
		if compareTS(m.TS, marks[searchWatermarkKey]) > 0 {
			marks[searchWatermarkKey] = m.TS
			advanced = true
		}
	}
	if advanced {
		if err := writeWatermarks(ctx, host, marks); err != nil {
			log.Printf("slack: persist watermarks: %v", err)
		}
	}
	return nil
}

// ensureProbed refreshes the auth-health fields when the probe is stale or the
// credentials changed.
func (s *supervisor) ensureProbed(ctx context.Context, cl *client, cfg *config, st *status) error {
	fingerprint := credentialFingerprint(cfg)
	s.mu.Lock()
	fresh := s.probedFor == fingerprint && time.Since(s.lastProbe) < probeInterval
	s.mu.Unlock()
	if fresh && st.CheckedAt != "" {
		return nil
	}
	res, err := cl.AuthTest(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.lastProbe = time.Now()
	s.probedFor = fingerprint
	s.mu.Unlock()

	st.OK = res.OK
	st.Error = res.Error
	st.CheckedAt = nowRFC3339()
	if res.OK {
		st.TeamName = res.TeamName
		st.TeamID = res.TeamID
		st.UserID = res.UserID
		st.UserName = res.DisplayName
	}
	return nil
}

// credentialFingerprint identifies a credential set without retaining it. Only
// lengths and the last few characters are used: enough to notice a rotation,
// never enough to reconstruct a secret if it reaches a log.
func credentialFingerprint(cfg *config) string {
	return fmt.Sprintf("%s|%d:%s|%d:%s|%d:%s",
		cfg.Mode,
		len(cfg.AppToken), tail(cfg.AppToken),
		len(cfg.BotToken), tail(cfg.BotToken),
		len(cfg.SessionToken), tail(cfg.SessionToken))
}

func tail(s string) string {
	const n = 4
	if len(s) <= n {
		return ""
	}
	return s[len(s)-n:]
}

// collect gathers session-fallback messages newer than the watermark.
func collect(ctx context.Context, cl *client, cfg *config, userID string, marks map[string]string) ([]message, error) {
	if userID == "" {
		return nil, errors.New("no authenticated Slack user id yet — the credential probe has not succeeded")
	}
	watermark := marks[searchWatermarkKey]
	var found []message
	for _, q := range searchQueries(cfg, userID) {
		matches, err := cl.SearchMessages(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("search Slack: %w", err)
		}
		for _, m := range matches {
			if compareTS(m.TS, watermark) <= 0 {
				continue
			}
			if !hasCommandPrefix(m.Text, cfg.CommandPrefix) {
				continue
			}
			found = append(found, m)
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		return compareTS(found[i].TS, found[j].TS) < 0
	})
	return found, nil
}

// searchQueries builds one query per configured channel, or a single
// workspace-wide query when no channels are set. Slack's `in:` modifiers
// combine with AND, so two channels in one query match nothing.
func searchQueries(cfg *config, userID string) []string {
	base := fmt.Sprintf("from:<@%s> %q", userID, cfg.CommandPrefix)
	if len(cfg.Channels) == 0 {
		return []string{base}
	}
	out := make([]string, 0, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		out = append(out, fmt.Sprintf("%s in:<#%s>", base, ch))
	}
	return out
}

// hasCommandPrefix reports whether text opens with the fallback command
// marker. The leading "> " strip handles Slack rendering a quoted message, and
// the delimiter check stops "!kandevish" from matching "!kandev".
func hasCommandPrefix(text, prefix string) bool {
	t := normalizeLeading(text)
	if !strings.HasPrefix(strings.ToLower(t), strings.ToLower(prefix)) {
		return false
	}
	rest := t[len(prefix):]
	if rest == "" {
		return true
	}
	switch rest[0] {
	case ' ', '\t', '\n', ':', ',':
		return true
	default:
		return false
	}
}

// stripCommandPrefix returns the instruction with the marker and any separator
// punctuation removed.
func stripCommandPrefix(text, prefix string) string {
	t := normalizeLeading(text)
	if len(t) >= len(prefix) && strings.EqualFold(t[:len(prefix)], prefix) {
		t = t[len(prefix):]
	}
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(t), ":, "))
}

func normalizeLeading(text string) string {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "> ")
	return strings.TrimSpace(t)
}
