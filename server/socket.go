package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Socket Mode is how a Slack app receives events without exposing a public
// HTTPS endpoint: apps.connections.open returns a short-lived wss:// URL, and
// Slack pushes every event down that socket. kandev normally runs on
// localhost, so the alternative — the Events API with a public request URL —
// is not available to most installs, and polling (what the in-tree
// integration did) is the workaround Socket Mode exists to remove.
//
// https://docs.slack.dev/apis/events-api/using-socket-mode/

// ackDeadline is Slack's window for acknowledging an envelope. Slack retries
// anything unacknowledged within three seconds, so the ack is sent before the
// work starts rather than after it.
const ackDeadline = 3 * time.Second

// socketReadTimeout bounds a silent connection. Slack sends a ping well inside
// this, so exceeding it means the socket is dead and should be redialled.
const socketReadTimeout = 90 * time.Second

// reconnectFloor and reconnectCeiling bound the redial backoff. Slack cycles
// connections regularly by design (it sends a `disconnect` frame first), so a
// reconnect is routine and must not be treated as an outage.
const (
	reconnectFloor   = 1 * time.Second
	reconnectCeiling = 60 * time.Second
)

// socketEnvelope is the Socket Mode frame wrapper. Every frame carries a type;
// the ones that need acknowledging also carry an envelope id.
type socketEnvelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Reason     string          `json:"reason,omitempty"`
}

// eventsAPIPayload is the `events_api` frame's payload.
type eventsAPIPayload struct {
	Event struct {
		Type     string `json:"type"`
		User     string `json:"user"`
		Text     string `json:"text"`
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
		Channel  string `json:"channel"`
		BotID    string `json:"bot_id"`
		Subtype  string `json:"subtype"`
	} `json:"event"`
}

// slashCommandPayload is the `slash_commands` frame's payload. Slack sends
// slash commands as form fields, but Socket Mode delivers them already decoded
// into JSON.
type slashCommandPayload struct {
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`
	Text      string `json:"text"`
	Command   string `json:"command"`
	// ResponseURL is a pre-authorized callback Slack issues per invocation. It
	// is how a command is answered without the bot being in the channel.
	ResponseURL string `json:"response_url"`
}

// openConnectionResponse is apps.connections.open's reply.
type openConnectionResponse struct {
	envelope
	URL string `json:"url"`
}

// OpenSocketConnection exchanges an app-level token for a WebSocket URL. It is
// the only call that uses the app token rather than the bot token.
func OpenSocketConnection(ctx context.Context, appToken string) (string, error) {
	c := newClient(appToken, "")
	var resp openConnectionResponse
	if err := c.post(ctx, "apps.connections.open", url.Values{}, &resp); err != nil {
		// This is the first thing the operator sees when a token is wrong, and
		// it is the one call that uses the app-level token rather than the bot
		// token — so the remedy has to name that token specifically instead of
		// falling through to the generic bot-token advice.
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			return "", errors.New(explainAppTokenError(apiErr.Message))
		}
		return "", err
	}
	if resp.URL == "" {
		return "", errors.New("slack returned no Socket Mode URL")
	}
	return resp.URL, nil
}

// socketListener maintains the Socket Mode connection and hands each inbound
// request to the shared triage path.
type socketListener struct {
	appToken string
	// handle processes one request. It runs off the read loop so a slow
	// triage — an agent call takes seconds — cannot delay the next ack.
	handle func(context.Context, inboundRequest)
	// onState reports connection transitions so the plugin page can show
	// whether the socket is actually up.
	onState func(connected bool, err error)
	// botUserID is the app's own user id, used to strip the leading mention
	// from `@Kandev do the thing` and to ignore the app's own messages.
	botUserID string
	// questions receives replies in clarification threads. Nil when
	// clarification threads are off; then thread messages are ignored and
	// every mention is triaged as before.
	questions threadRouter
}

// Run keeps a connection open until ctx is cancelled, redialling with backoff.
func (l *socketListener) Run(ctx context.Context) {
	backoff := reconnectFloor
	for {
		if ctx.Err() != nil {
			return
		}
		err := l.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			l.reportState(false, err)
			log.Printf("slack: socket connection ended: %v", err)
		} else {
			// A clean end is Slack cycling the connection on purpose;
			// redial immediately rather than backing off.
			l.reportState(false, nil)
			backoff = reconnectFloor
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if err != nil {
			backoff = min(backoff*2, reconnectCeiling)
		}
	}
}

func (l *socketListener) reportState(connected bool, err error) {
	if l.onState != nil {
		l.onState(connected, err)
	}
}

// connectOnce opens one WebSocket and reads until it closes. A nil return
// means Slack asked to cycle the connection, which is routine.
func (l *socketListener) connectOnce(ctx context.Context) error {
	url, err := OpenSocketConnection(ctx, l.appToken)
	if err != nil {
		return fmt.Errorf("open socket connection: %w", err)
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dial socket: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Close the socket when the context ends so a blocked ReadMessage
	// unblocks instead of holding the loop open until the read timeout.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	l.reportState(true, nil)
	return l.readLoop(ctx, conn)
}

func (l *socketListener) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		if err := conn.SetReadDeadline(time.Now().Add(socketReadTimeout)); err != nil {
			return err
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var env socketEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			log.Printf("slack: undecodable socket frame: %v", err)
			continue
		}
		switch env.Type {
		case "hello":
			continue
		case "disconnect":
			// Slack cycles connections deliberately (refresh_requested) and
			// warns before closing. Either way the caller redials.
			return nil
		case "events_api", "slash_commands":
			l.ack(conn, env.EnvelopeID)
			l.dispatch(ctx, env)
		default:
			// Unknown frame types are acknowledged so Slack does not retry
			// them, but otherwise ignored — the type set grows over time.
			l.ack(conn, env.EnvelopeID)
		}
	}
}

// ack confirms an envelope. Slack redelivers anything unacknowledged within
// ackDeadline, so this is sent before any processing begins.
func (l *socketListener) ack(conn *websocket.Conn, envelopeID string) {
	if envelopeID == "" {
		return
	}
	if err := conn.SetWriteDeadline(time.Now().Add(ackDeadline)); err != nil {
		log.Printf("slack: set ack deadline: %v", err)
		return
	}
	if err := conn.WriteJSON(map[string]string{"envelope_id": envelopeID}); err != nil {
		log.Printf("slack: ack failed: %v", err)
	}
}

// dispatch turns a frame into an inboundRequest and hands it off. Processing
// runs in its own goroutine so the read loop stays responsive to the next ack.
func (l *socketListener) dispatch(ctx context.Context, env socketEnvelope) {
	if env.Type == "events_api" {
		if msg, ok := l.decodeThreadMessage(env.Payload); ok {
			if l.questions != nil {
				go l.questions.HandleThreadReply(ctx, msg)
			}
			return
		}
	}
	req, ok := l.decode(env)
	if !ok {
		return
	}
	if l.questions != nil && req.ThreadTS != "" && req.ThreadTS != req.TS {
		// "@Kandev <answer>" inside a clarification thread is an answer, not
		// a new request. The same reply may also arrive as a message event;
		// the bridge answers it once.
		go func() {
			if l.questions.OwnsThread(ctx, req.ChannelID, req.ThreadTS) {
				l.questions.HandleThreadReply(ctx, threadMessage{
					Channel: req.ChannelID, TS: req.TS, ThreadTS: req.ThreadTS,
					User: req.UserID, Text: req.Instruction,
				})
				return
			}
			l.handle(ctx, req)
		}()
		return
	}
	go l.handle(ctx, req)
}

// decodeThreadMessage picks out a person's reply inside a thread, delivered
// by the message.channels / message.groups subscriptions. Edits, joins, bot
// posts (including this app's own), and thread parents are skipped.
func (l *socketListener) decodeThreadMessage(payload json.RawMessage) (threadMessage, bool) {
	var p eventsAPIPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return threadMessage{}, false
	}
	e := p.Event
	if e.Type != "message" || e.Subtype != "" || e.BotID != "" {
		return threadMessage{}, false
	}
	if e.ThreadTS == "" || e.ThreadTS == e.TS || e.User == "" {
		return threadMessage{}, false
	}
	if l.botUserID != "" && e.User == l.botUserID {
		return threadMessage{}, false
	}
	return threadMessage{Channel: e.Channel, TS: e.TS, ThreadTS: e.ThreadTS, User: e.User, Text: e.Text}, true
}

func (l *socketListener) decode(env socketEnvelope) (inboundRequest, bool) {
	switch env.Type {
	case "events_api":
		return l.decodeEvent(env.Payload)
	case "slash_commands":
		return decodeSlashCommand(env.Payload)
	default:
		return inboundRequest{}, false
	}
}

// decodeEvent handles app_mention. Other event types are ignored rather than
// rejected: the manifest only subscribes to app_mention, but Slack may deliver
// housekeeping events on the same socket.
func (l *socketListener) decodeEvent(payload json.RawMessage) (inboundRequest, bool) {
	var p eventsAPIPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		log.Printf("slack: undecodable event payload: %v", err)
		return inboundRequest{}, false
	}
	e := p.Event
	if e.Type != "app_mention" {
		return inboundRequest{}, false
	}
	// Never act on the app's own posts: the reply mentions nobody, but a
	// future change that echoes a mention would otherwise loop forever.
	if e.BotID != "" || (l.botUserID != "" && e.User == l.botUserID) {
		return inboundRequest{}, false
	}
	instruction := stripMention(e.Text, l.botUserID)
	if instruction == "" {
		return inboundRequest{}, false
	}
	return inboundRequest{
		ChannelID:   e.Channel,
		TS:          e.TS,
		ThreadTS:    e.ThreadTS,
		UserID:      e.User,
		Text:        e.Text,
		Instruction: instruction,
		Acknowledge: true,
	}, true
}

// decodeSlashCommand handles /kandev. A slash command has no message of its
// own in the channel, so there is nothing to react to; the reply goes back
// through the command's response_url instead of into the channel.
func decodeSlashCommand(payload json.RawMessage) (inboundRequest, bool) {
	var p slashCommandPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		log.Printf("slack: undecodable slash command payload: %v", err)
		return inboundRequest{}, false
	}
	instruction := strings.TrimSpace(p.Text)
	if instruction == "" {
		return inboundRequest{}, false
	}
	return inboundRequest{
		ChannelID:   p.ChannelID,
		UserID:      p.UserID,
		UserName:    p.UserName,
		Text:        p.Command + " " + instruction,
		Instruction: instruction,
		ResponseURL: p.ResponseURL,
		Acknowledge: false,
	}, true
}

// stripMention removes the leading `<@BOTID>` Slack puts at the start of a
// mention, leaving the instruction. A mention elsewhere in the sentence is
// left alone — it is part of what the user wrote.
func stripMention(text, botUserID string) string {
	trimmed := strings.TrimSpace(text)
	if botUserID != "" {
		for _, form := range []string{"<@" + botUserID + ">", "<@" + botUserID + "|"} {
			if idx := strings.Index(trimmed, form); idx == 0 {
				rest := trimmed[len(form):]
				// The `<@ID|label>` form still has the label and closing
				// bracket to discard.
				if strings.HasSuffix(form, "|") {
					if close := strings.Index(rest, ">"); close >= 0 {
						rest = rest[close+1:]
					}
				}
				trimmed = rest
				break
			}
		}
	}
	trimmed = strings.TrimSpace(trimmed)
	return strings.TrimSpace(strings.TrimLeft(trimmed, ":, "))
}

// explainAppTokenError expands the codes apps.connections.open returns. The
// bot token and the app-level token are pasted into adjacent fields and both
// come from the same Slack app, so a failure here has to say which one Slack
// rejected.
func explainAppTokenError(code string) string {
	switch code {
	case "invalid_auth", "not_authed":
		return code + " — the app-level token was rejected. Generate one under Basic Information > App-Level Tokens (not OAuth & Permissions, which is where the bot token lives)."
	case "missing_scope":
		return "missing_scope — the app-level token needs the connections:write scope. Generate a new one with that scope; scopes cannot be added to an existing app-level token."
	case "not_allowed_token_type":
		return "not_allowed_token_type — this is not an app-level token. Socket Mode needs the xapp- token from Basic Information > App-Level Tokens."
	case "socket_mode_not_enabled":
		return "socket_mode_not_enabled — turn Socket Mode on in the app's settings, or re-apply the plugin's slack-app-manifest.yaml, which enables it."
	default:
		return code
	}
}
