package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// slackPlugin implements the Kandev plugin surface. The Slack work itself
// runs on a timer rather than in response to bus events, so OnEvent stays the
// embedded no-op and the webhook keys exist only to serve the plugin's own UI
// bundle.
type slackPlugin struct {
	pluginsdk.UnimplementedPlugin

	supervisor *supervisor

	startOnce sync.Once
	// A single managed process owns delivery. The channel makes waiting for
	// its state transaction cancellable under the host's 30-second deadline.
	notifyOnce sync.Once
	notifyGate chan struct{}
	// runCtx bounds the polling loop. Kandev owns the subprocess lifecycle
	// and kills it on disable/uninstall, so cancellation here is only needed
	// for tests and an orderly shutdown.
	runCtx context.Context
}

func newSlackPlugin(ctx context.Context) *slackPlugin {
	p := &slackPlugin{runCtx: ctx}
	p.supervisor = newSupervisor(func() pluginsdk.Host { return p.Host() })
	return p
}

// SetHost starts the source supervisor once the broker connection to Kandev is
// live. Serve injects the Host from a background goroutine after startup, so
// this is the earliest point at which any Host call can succeed — starting
// the loop in main would just spin against a nil Host.
func (p *slackPlugin) SetHost(h pluginsdk.Host) {
	p.UnimplementedPlugin.SetHost(h)
	p.startOnce.Do(func() {
		go p.supervisor.Run(p.runCtx)
	})
}

// HandleWebhook serves the plugin page's three relays. Kandev routes only
// declared keys here, but it does not enforce the manifest's method field, so
// the mutating keys check it themselves.
func (p *slackPlugin) HandleWebhook(ctx context.Context, req *pluginsdk.WebhookRequest) (*pluginsdk.WebhookResponse, error) {
	if req == nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "empty request"})
	}
	switch req.WebhookKey {
	case "status":
		return p.handleStatus(ctx)
	case "test":
		if !isPost(req) {
			return methodNotAllowed()
		}
		return p.handleTest(ctx)
	case "scan":
		if !isPost(req) {
			return methodNotAllowed()
		}
		p.supervisor.ScanNow()
		return jsonResponse(http.StatusAccepted, map[string]any{"scheduled": true})
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown webhook"})
	}
}

// handleStatus returns the persisted health plus the currently configured
// (non-secret) settings, so the page can explain what the plugin is watching
// without the operator cross-referencing Settings > Plugins.
func (p *slackPlugin) handleStatus(ctx context.Context) (*pluginsdk.WebhookResponse, error) {
	host := p.Host()
	if host == nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": "plugin host unavailable"})
	}
	st := readStatus(ctx, host)
	payload := map[string]any{
		"configured": st.Configured,
		"ok":         st.OK,
		"error":      st.Error,
		"mode":       st.Mode,
		"modeLabel":  st.ModeLabel,
		"teamName":   st.TeamName,
		"userName":   st.UserName,
		"checkedAt":  st.CheckedAt,
		"scannedAt":  st.ScannedAt,
		"triaged":    st.Triaged,
		"recent":     st.Recent,
		"questions":  readQuestionStatus(ctx, host),
	}
	if cfg, err := p.currentConfig(ctx, host); err == nil {
		payload["startAgent"] = cfg.StartAgent
		payload["realtime"] = cfg.Mode.realtime()
		if !cfg.Mode.realtime() {
			payload["commandPrefix"] = cfg.CommandPrefix
			payload["channels"] = cfg.Channels
			payload["pollIntervalSeconds"] = int(cfg.PollInterval.Seconds())
		}
	} else if !st.Configured {
		payload["error"] = configErrorMessage(err)
	}
	return jsonResponse(http.StatusOK, payload)
}

// handleTest probes the stored credentials on demand. It reports the result
// without persisting it: a manual test is a diagnostic, and letting it
// overwrite the poller's health record would make an intermittent failure
// look resolved.
func (p *slackPlugin) handleTest(ctx context.Context) (*pluginsdk.WebhookResponse, error) {
	host := p.Host()
	if host == nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": "plugin host unavailable"})
	}
	cfg, err := p.currentConfig(ctx, host)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": configErrorMessage(err),
		})
	}
	token, cookie := cfg.WebCredentials()
	res, err := newClient(token, cookie).AuthTest(ctx)
	if err != nil {
		return jsonResponse(http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"ok":        res.OK,
		"error":     res.Error,
		"teamName":  res.TeamName,
		"userName":  res.DisplayName,
		"url":       res.URL,
		"mode":      cfg.Mode.String(),
		"modeLabel": cfg.Mode.Label(),
	})
}

func (p *slackPlugin) currentConfig(ctx context.Context, host pluginsdk.Host) (*config, error) {
	raw, err := host.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	return loadConfig(raw)
}

// configErrorMessage turns the "nothing filled in yet" sentinel into copy an
// operator can act on, and passes real validation errors through unchanged.
func configErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(err.Error(), errNotConfigured.Error()) {
		return "Add your Slack app tokens and pick a triage agent in Settings > Plugins."
	}
	return err.Error()
}

func isPost(req *pluginsdk.WebhookRequest) bool {
	return strings.EqualFold(req.Method, http.MethodPost)
}

func methodNotAllowed() (*pluginsdk.WebhookResponse, error) {
	return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
}

func jsonResponse(status int, payload map[string]any) (*pluginsdk.WebhookResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return &pluginsdk.WebhookResponse{
			Status:  http.StatusInternalServerError,
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    []byte(`{"error":"encode response"}`),
		}, nil
	}
	return &pluginsdk.WebhookResponse{
		Status:  int32(status),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    body,
	}, nil
}
