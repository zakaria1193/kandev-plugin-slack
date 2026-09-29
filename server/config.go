package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// authMode is how the plugin reaches Slack. It is derived from which
// credentials the operator filled in rather than configured separately: Slack
// token prefixes are unambiguous, and a mode picker only creates a second
// source of truth that can disagree with the pasted credential.
type authMode string

const (
	// authModeApp is a real Slack app running in Socket Mode: an app-level
	// token opens a WebSocket to Slack and a bot token does the reading and
	// writing. This is what a Slack integration normally looks like — events
	// arrive the instant they happen, there is nothing to poll, and no public
	// URL is required, which matters because kandev usually runs on localhost.
	authModeApp authMode = "app"

	// authModeSession is the browser-session fallback: the xoxc- token and `d`
	// cookie the Slack web client uses. Unofficial and unsupported by Slack,
	// and it cannot receive events, so it polls search.messages. It exists for
	// locked-down workspaces where admins do not permit app installs at all —
	// the case the in-tree integration was originally built for.
	authModeSession authMode = "session"
)

// String renders the mode for status output and error messages.
func (m authMode) String() string { return string(m) }

// Label is the human-facing name shown on the plugin page.
func (m authMode) Label() string {
	switch m {
	case authModeApp:
		return "Slack app (Socket Mode)"
	case authModeSession:
		return "Browser session (fallback)"
	default:
		return string(m)
	}
}

// realtime reports whether the mode receives events pushed by Slack. Only the
// session fallback has to poll.
func (m authMode) realtime() bool { return m == authModeApp }

// Defaults mirror the values declared in manifest.yaml's config_schema. They
// are repeated here because GetConfig returns the stored config, and a field
// the operator never touched is absent rather than defaulted.
const (
	defaultCommandPrefix       = "!kandev"
	defaultPollIntervalSeconds = 30
	minPollIntervalSeconds     = 5
	maxPollIntervalSeconds     = 600
)

// config is the validated view of the operator's plugin configuration.
type config struct {
	Mode authMode

	// AppToken (xapp-) opens the Socket Mode WebSocket; BotToken (xoxb-) is
	// used for every Web API call and is the identity replies come from.
	AppToken string
	BotToken string

	// SessionToken (xoxc-) and SessionCookie (`d`) are the fallback pair.
	SessionToken  string
	SessionCookie string

	// CommandPrefix, Channels and PollInterval only apply to the session
	// fallback. A Slack app is addressed by mentioning it or by its slash
	// command, so it needs no prefix, and it is pushed events for the channels
	// it belongs to, so it needs no channel list or cadence.
	CommandPrefix string
	Channels      []string
	PollInterval  time.Duration

	StartAgent   bool
	UtilityAgent string

	// Questions configures clarification threads; nil when they are off.
	// QuestionsErr explains why they are on but cannot run. Neither blocks
	// triage.
	Questions    *questionsConfig
	QuestionsErr error
}

// WebCredentials returns the (token, cookie) pair for Slack Web API calls.
// A bot token authenticates on its own, so no cookie travels with it —
// sending one alongside an xoxb- token makes Slack reject the call rather
// than ignore the extra header.
func (c *config) WebCredentials() (string, string) {
	if c.Mode == authModeApp {
		return c.BotToken, ""
	}
	return c.SessionToken, c.SessionCookie
}

// errNotConfigured is the sentinel for "the operator has not finished filling
// in Settings > Plugins yet". Callers treat it as an idle state, not a failure
// worth logging on every tick.
var errNotConfigured = errors.New("slack plugin is not configured")

// loadConfig validates the raw config_schema values Host.GetConfig returns.
// kandev already enforces required-ness and types; what is left here is the
// cross-field logic a JSON-Schema subset cannot express — which credential
// combinations form a coherent install path.
func loadConfig(raw map[string]any) (*config, error) {
	cfg := &config{
		AppToken:      strings.TrimSpace(configString(raw, "app_token")),
		BotToken:      strings.TrimSpace(configString(raw, "bot_token")),
		SessionToken:  strings.TrimSpace(configString(raw, "session_token")),
		SessionCookie: strings.TrimSpace(configString(raw, "session_cookie")),
		CommandPrefix: strings.TrimSpace(configString(raw, "command_prefix")),
		Channels:      parseChannels(configString(raw, "channels")),
		PollInterval:  pollInterval(raw),
		StartAgent:    configBool(raw, "start_agent"),
		UtilityAgent:  strings.TrimSpace(configString(raw, "utility_agent")),
	}
	if cfg.CommandPrefix == "" {
		cfg.CommandPrefix = defaultCommandPrefix
	}
	mode, err := detectMode(cfg)
	if err != nil {
		return nil, err
	}
	cfg.Mode = mode
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.Questions, cfg.QuestionsErr = loadQuestionsConfig(raw, cfg.Mode)
	return cfg, nil
}

// detectMode picks the install path from the credentials present. The app path
// wins when both are filled: it is the supported one, and silently preferring
// the fallback because a stale cookie is still saved would be a confusing way
// to lose real-time events.
func detectMode(cfg *config) (authMode, error) {
	hasApp := cfg.AppToken != "" || cfg.BotToken != ""
	hasSession := cfg.SessionToken != "" || cfg.SessionCookie != ""
	switch {
	case hasApp:
		return authModeApp, nil
	case hasSession:
		return authModeSession, nil
	default:
		return "", errNotConfigured
	}
}

func (c *config) validate() error {
	if c.UtilityAgent == "" {
		return errors.New("pick a triage agent")
	}
	if c.Mode == authModeApp {
		return c.validateApp()
	}
	return c.validateSession()
}

// validateApp checks the Socket Mode pair. Both tokens are required and they
// are easy to swap, so a wrong-prefix value is named rather than left to fail
// as an opaque Slack rejection later.
func (c *config) validateApp() error {
	if c.AppToken == "" {
		return errors.New("a Slack app needs its app-level token too — create one with the connections:write scope under Basic Information > App-Level Tokens")
	}
	if c.BotToken == "" {
		return errors.New("a Slack app needs its bot token too — copy the Bot User OAuth Token from OAuth & Permissions after installing the app")
	}
	if !strings.HasPrefix(c.AppToken, appTokenPrefix) {
		return fmt.Errorf("the app-level token must start with %q — %s", appTokenPrefix, describeToken(c.AppToken))
	}
	if !strings.HasPrefix(c.BotToken, botTokenPrefix) {
		return fmt.Errorf("the bot token must start with %q — %s", botTokenPrefix, describeToken(c.BotToken))
	}
	return nil
}

func (c *config) validateSession() error {
	if c.SessionToken == "" {
		return errors.New("the browser-session fallback needs the xoxc- token as well as the cookie")
	}
	if c.SessionCookie == "" {
		return errors.New("the browser-session fallback needs the `d` cookie as well as the token — copy both from the same logged-in Slack tab")
	}
	if !strings.HasPrefix(c.SessionToken, sessionTokenPrefix) {
		return fmt.Errorf("the browser-session token must start with %q — %s", sessionTokenPrefix, describeToken(c.SessionToken))
	}
	if strings.HasPrefix(c.CommandPrefix, "/") {
		return errors.New("command prefix cannot start with \"/\" — Slack intercepts slash commands before they become messages")
	}
	return nil
}

// Slack's documented token prefixes. Kept as constants because the same
// strings appear in validation, detection, and operator-facing errors.
const (
	appTokenPrefix     = "xapp-"
	botTokenPrefix     = "xoxb-"
	userTokenPrefix    = "xoxp-"
	sessionTokenPrefix = "xoxc-"
	refreshTokenPrefix = "xoxe-"
)

// describeToken names what the operator actually pasted. Every one of these is
// reachable from the same Slack app page as the token that was wanted, so
// saying which one it is saves a round of guessing.
func describeToken(token string) string {
	switch {
	case strings.HasPrefix(token, appTokenPrefix):
		return "this is an app-level token"
	case strings.HasPrefix(token, botTokenPrefix):
		return "this is a bot token"
	case strings.HasPrefix(token, userTokenPrefix):
		return "this is a user token; this plugin uses the bot token instead"
	case strings.HasPrefix(token, sessionTokenPrefix):
		return "this is a browser-session token; it belongs in the fallback fields"
	case strings.HasPrefix(token, refreshTokenPrefix):
		return "this is a refresh token, not an access token"
	default:
		return "this does not look like a Slack token"
	}
}

// parseChannels splits the comma-separated channel list, tolerating the spaces
// and #-prefixes operators paste in. Channel *names* cannot be resolved
// without an extra API round-trip per poll, so only IDs are accepted; a stray
// "#" is stripped rather than silently producing a channel that never matches.
func parseChannels(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		id := strings.TrimPrefix(strings.TrimSpace(f), "#")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// pollInterval clamps the configured cadence into the supported band.
// Out-of-range values fall back to the default rather than erroring: a
// too-eager interval is a tuning mistake, not a reason to stop triaging.
func pollInterval(raw map[string]any) time.Duration {
	seconds := configInt(raw, "poll_interval_seconds")
	if seconds < minPollIntervalSeconds || seconds > maxPollIntervalSeconds {
		seconds = defaultPollIntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

func configString(raw map[string]any, key string) string {
	if raw == nil {
		return ""
	}
	s, _ := raw[key].(string)
	return s
}

func configBool(raw map[string]any, key string) bool {
	if raw == nil {
		return false
	}
	b, _ := raw[key].(bool)
	return b
}

// configInt reads a numeric config value. Numbers cross the Host gRPC boundary
// as protobuf Struct values, which are always float64 on arrival — the integer
// cases are for direct construction in tests.
func configInt(raw map[string]any, key string) int {
	if raw == nil {
		return 0
	}
	switch v := raw[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return 0
	}
}
