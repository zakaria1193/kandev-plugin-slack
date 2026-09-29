package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// fakeRouter records what the listener hands to the clarification bridge.
type fakeRouter struct {
	owned   string
	replies chan threadMessage
}

func (f *fakeRouter) OwnsThread(_ context.Context, channel, threadTS string) bool {
	return channel+"/"+threadTS == f.owned
}

func (f *fakeRouter) HandleThreadReply(_ context.Context, msg threadMessage) { f.replies <- msg }

func eventEnvelope(t *testing.T, event map[string]any) socketEnvelope {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"event": event})
	if err != nil {
		t.Fatal(err)
	}
	return socketEnvelope{Type: "events_api", EnvelopeID: "env", Payload: payload}
}

func TestDecodeThreadMessage(t *testing.T) {
	l := &socketListener{botUserID: "U0BOT"}
	reply := map[string]any{"type": "message", "user": "U1", "text": "1. yes", "ts": "2.0", "thread_ts": "1.0", "channel": "C1"}
	msg, ok := l.decodeThreadMessage(eventEnvelope(t, reply).Payload)
	if !ok || msg.Channel != "C1" || msg.TS != "2.0" || msg.ThreadTS != "1.0" || msg.User != "U1" || msg.Text != "1. yes" {
		t.Fatalf("decoded %+v, %v", msg, ok)
	}

	skip := map[string]map[string]any{
		"thread parent":  {"type": "message", "user": "U1", "text": "x", "ts": "1.0", "thread_ts": "1.0", "channel": "C1"},
		"not threaded":   {"type": "message", "user": "U1", "text": "x", "ts": "2.0", "channel": "C1"},
		"edit":           {"type": "message", "subtype": "message_changed", "ts": "2.0", "thread_ts": "1.0", "channel": "C1"},
		"other bot":      {"type": "message", "bot_id": "B1", "user": "U2", "text": "x", "ts": "2.0", "thread_ts": "1.0", "channel": "C1"},
		"this app":       {"type": "message", "user": "U0BOT", "text": "x", "ts": "2.0", "thread_ts": "1.0", "channel": "C1"},
		"mention":        {"type": "app_mention", "user": "U1", "text": "<@U0BOT> x", "ts": "2.0", "thread_ts": "1.0", "channel": "C1"},
		"no user at all": {"type": "message", "text": "x", "ts": "2.0", "thread_ts": "1.0", "channel": "C1"},
	}
	for name, event := range skip {
		if _, ok := l.decodeThreadMessage(eventEnvelope(t, event).Payload); ok {
			t.Fatalf("%s: decoded as a reply", name)
		}
	}
}

func waitFor[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	var zero T
	return zero
}

func expectNone[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("unexpected %+v", v)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDispatchSendsThreadRepliesToTheBridge(t *testing.T) {
	router := &fakeRouter{replies: make(chan threadMessage, 2)}
	triaged := make(chan inboundRequest, 2)
	l := &socketListener{botUserID: "U0BOT", questions: router,
		handle: func(_ context.Context, req inboundRequest) { triaged <- req }}

	l.dispatch(context.Background(), eventEnvelope(t, map[string]any{
		"type": "message", "user": "U1", "text": "sure", "ts": "2.0", "thread_ts": "1.0", "channel": "C1"}))

	if got := waitFor(t, router.replies); got.Text != "sure" {
		t.Fatalf("reply = %+v", got)
	}
	expectNone(t, triaged)
}

func TestDispatchMentionInAQuestionThreadAnswersInsteadOfTriaging(t *testing.T) {
	router := &fakeRouter{owned: "C1/1.0", replies: make(chan threadMessage, 2)}
	triaged := make(chan inboundRequest, 2)
	l := &socketListener{botUserID: "U0BOT", questions: router,
		handle: func(_ context.Context, req inboundRequest) { triaged <- req }}

	l.dispatch(context.Background(), eventEnvelope(t, map[string]any{
		"type": "app_mention", "user": "U1", "text": "<@U0BOT> 1. yes", "ts": "2.0", "thread_ts": "1.0", "channel": "C1"}))

	got := waitFor(t, router.replies)
	if got.Text != "1. yes" || got.ThreadTS != "1.0" || got.User != "U1" {
		t.Fatalf("reply = %+v", got)
	}
	expectNone(t, triaged)
}

func TestDispatchMentionInAnotherThreadStillTriages(t *testing.T) {
	router := &fakeRouter{owned: "C1/9.0", replies: make(chan threadMessage, 2)}
	triaged := make(chan inboundRequest, 2)
	l := &socketListener{botUserID: "U0BOT", questions: router,
		handle: func(_ context.Context, req inboundRequest) { triaged <- req }}

	l.dispatch(context.Background(), eventEnvelope(t, map[string]any{
		"type": "app_mention", "user": "U1", "text": "<@U0BOT> file this", "ts": "2.0", "thread_ts": "1.0", "channel": "C1"}))

	if got := waitFor(t, triaged); got.Instruction != "file this" {
		t.Fatalf("triaged %+v", got)
	}
	expectNone(t, router.replies)
}

func TestDispatchWithoutABridgeIgnoresThreadMessages(t *testing.T) {
	triaged := make(chan inboundRequest, 2)
	l := &socketListener{botUserID: "U0BOT", handle: func(_ context.Context, req inboundRequest) { triaged <- req }}

	l.dispatch(context.Background(), eventEnvelope(t, map[string]any{
		"type": "message", "user": "U1", "text": "sure", "ts": "2.0", "thread_ts": "1.0", "channel": "C1"}))
	expectNone(t, triaged)

	l.dispatch(context.Background(), eventEnvelope(t, map[string]any{
		"type": "app_mention", "user": "U1", "text": "<@U0BOT> file this", "ts": "2.0", "thread_ts": "1.0", "channel": "C1"}))
	waitFor(t, triaged)
}
