package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wavever/CCLimitPing/internal/activity"
	"github.com/wavever/CCLimitPing/internal/auth"
	"github.com/wavever/CCLimitPing/internal/config"
	"github.com/wavever/CCLimitPing/internal/terminal"
	"github.com/wavever/CCLimitPing/internal/usage"
)

const (
	claudeUsageURL       = "https://api.anthropic.com/api/oauth/usage"
	claudeCountTokensURL = "https://api.anthropic.com/v1/messages/count_tokens"
	claudeOAuthBeta      = "oauth-2025-04-20"
	claudeAPIVersion     = "2023-06-01"
	claudeFallbackVer    = "2.1.0"
	claudeFiveHourSec    = 5 * 60 * 60
	claudeWeeklySec      = 7 * 24 * 60 * 60

	// The usage endpoint collapses a disabled subscription into a generic 429.
	// Token counting is free, creates no Message, and has an independent rate
	// limit, so it is a safe authorization probe when that 429 leaves the account
	// state ambiguous.
	claudeAccessProbeBody = `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"."}]}`
	claudeOAuthOrgDenied  = "OAuth authentication is currently not allowed for this organization"
	claudeOAuthOrgCode    = "oauth_org_not_allowed"
	claudeDisabledText    = "Your organization has disabled Claude subscription access for Claude Code"
	claudeProbeTimeout    = 10 * time.Second

	// Interactive-trigger timing. The 5h window anchors when the submitted
	// prompt's request is dispatched, so we wait for the TUI to render and
	// settle, submit the prompt, let the turn run until its output goes quiet,
	// then exit cleanly — exiting before the request dispatches leaves the
	// window unstarted (the original "wait 2s then /exit" bug).
	claudeStartupTimeout = 10 * time.Second
	claudeStartupSettle  = 1200 * time.Millisecond
	claudeTurnMinWait    = 4 * time.Second
	claudeTurnQuiet      = 2500 * time.Millisecond
	claudeTurnMaxWait    = 45 * time.Second
	claudeExitGrace      = 5 * time.Second
	claudePollInterval   = 200 * time.Millisecond
)

var (
	claudeUserAgentOnce sync.Once
	claudeUserAgent     string
	claudeANSIEscapeRE  = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	claudeNonTextRE     = regexp.MustCompile(`[^a-z0-9_]+`)
)

// ClaudeSubscriptionAccessError means Anthropic accepted the OAuth identity
// but explicitly rejected Claude Code subscription authentication. It wraps the
// failure it was diagnosed from (if any), so status-aware callers — e.g. the
// scheduler honoring a usage 429's Retry-After — keep seeing it.
type ClaudeSubscriptionAccessError struct{ Err error }

func (*ClaudeSubscriptionAccessError) Error() string {
	return "Claude subscription access is unavailable (the plan may have expired, or an organization admin may have disabled Claude Code); renew or re-enable the subscription, or use an Anthropic API key in Claude Code"
}

func (e *ClaudeSubscriptionAccessError) Unwrap() error { return e.Err }

// Claude reads usage via the OAuth usage endpoint and triggers windows via the
// interactive, TTY-backed Claude Code CLI. Print mode is intentionally avoided
// because it is billed through Agent SDK/API credits rather than Claude
// subscription limits.
type Claude struct {
	cfg  config.ProviderConfig
	auth *auth.ClaudeAuth
}

func NewClaude(cfg config.ProviderConfig) *Claude {
	return &Claude{cfg: cfg, auth: auth.NewClaudeAuth()}
}

func (c *Claude) Name() string { return "claude" }

func (c *Claude) ActiveTask(_ context.Context) (string, bool, error) {
	// Active-session detection relies entirely on the CLI hooks (see `limitping
	// hooks install`). Without them we don't guess from the process list — the
	// scheduler just pings.
	if !activity.Enabled("claude") {
		return "", false, nil
	}
	return activity.Active("claude")
}

type claudeWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type claudeUsageResp struct {
	FiveHour claudeWindow `json:"five_hour"`
	SevenDay claudeWindow `json:"seven_day"`
}

func (c *Claude) ReadUsage(ctx context.Context) (*usage.Usage, error) {
	body, err := fetchWithAuth(ctx, c.auth, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-beta", claudeOAuthBeta)
		req.Header.Set("User-Agent", claudeCodeUserAgent())
		return req, nil
	})
	if err != nil {
		return nil, diagnoseClaudeUsageError(ctx, c.auth, err)
	}

	var r claudeUsageResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("claude usage: parsing response: %w", err)
	}

	u := &usage.Usage{
		Provider:  "claude",
		FetchedAt: time.Now(),
		Raw:       body,
		FiveHour: usage.Window{
			UsedPercent:   r.FiveHour.Utilization,
			ResetsAt:      parseTime(r.FiveHour.ResetsAt),
			WindowSeconds: claudeFiveHourSec,
		},
		Weekly: usage.Window{
			UsedPercent:   r.SevenDay.Utilization,
			ResetsAt:      parseTime(r.SevenDay.ResetsAt),
			WindowSeconds: claudeWeeklySec,
		},
	}
	u.LimitReached = u.FiveHour.UsedPercent >= 100 || u.Weekly.UsedPercent >= 100
	return u, nil
}

// diagnoseClaudeUsageError resolves the ambiguity unique to Claude's OAuth
// usage endpoint: a disabled subscription can be returned as the same generic
// 429 used for a real endpoint throttle. Any inconclusive probe deliberately
// preserves the original error, preventing false subscription warnings.
func diagnoseClaudeUsageError(ctx context.Context, src tokenSource, usageErr error) error {
	var httpErr *UsageHTTPError
	if !errors.As(usageErr, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
		return usageErr
	}
	if claudeSubscriptionAccessUnavailable(ctx, src) {
		return &ClaudeSubscriptionAccessError{Err: usageErr}
	}
	return usageErr
}

// claudeSubscriptionAccessUnavailable checks the inference authorization gate
// through Anthropic's zero-cost token-counting endpoint. The token was just
// accepted by the usage endpoint (a 429 is not an auth failure), so no
// reload/refresh ladder is needed here: anything but an explicit denial is
// inconclusive and leaves the original error untouched.
func claudeSubscriptionAccessUnavailable(ctx context.Context, src tokenSource) bool {
	probeCtx, cancel := context.WithTimeout(ctx, claudeProbeTimeout)
	defer cancel()

	token, err := src.Token(probeCtx)
	if err != nil || token == "" {
		return false
	}
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, claudeCountTokensURL,
		strings.NewReader(claudeAccessProbeBody))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", claudeAPIVersion)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	req.Header.Set("User-Agent", claudeCodeUserAgent())

	resp, err := usageHTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return false
	}
	return claudeSubscriptionDeniedResponse(resp.StatusCode, body)
}

func claudeSubscriptionDeniedResponse(status int, body []byte) bool {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	return claudeAccessDenied(string(body))
}

// claudeAccessDenied matches Anthropic's subscription-denial wording in both an
// API error body and Claude Code's own rendered output. Everything that is not
// alphanumeric is collapsed first — ANSI escapes, JSON punctuation, and the
// line breaks and box borders the TUI injects when it wraps these sentences —
// so one matcher serves both and neither casing nor wrapping defeats it.
func claudeAccessDenied(text string) bool {
	plain := claudeNormalizedText(text)
	for _, denial := range []string{claudeDisabledText, claudeOAuthOrgDenied, claudeOAuthOrgCode} {
		if strings.Contains(plain, claudeNormalizedText(denial)) {
			return true
		}
	}
	return false
}

func claudeNormalizedText(text string) string {
	stripped := claudeANSIEscapeRE.ReplaceAllString(text, " ")
	return claudeNonTextRE.ReplaceAllString(strings.ToLower(stripped), " ")
}

func claudeCodeUserAgent() string {
	claudeUserAgentOnce.Do(func() {
		claudeUserAgent = "claude-code/" + claudeFallbackVer

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		out, err := exec.CommandContext(ctx, "claude", "--version").Output()
		if err != nil {
			return
		}
		if version := normalizedClaudeVersion(string(out)); version != "" {
			claudeUserAgent = "claude-code/" + version
		}
	})
	return claudeUserAgent
}

func normalizedClaudeVersion(raw string) string {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func (c *Claude) Trigger(ctx context.Context, dryRun bool) (*TriggerResult, error) {
	prompt := c.cfg.Prompt
	if prompt == "" {
		prompt = "."
	}
	args := []string{}
	if c.cfg.Model != "" {
		args = append(args, "--model", c.cfg.Model)
	}
	args = append(args, claudeInteractiveArgs(c.cfg.ExtraArgs)...)
	args = append(args, prompt)

	res := &TriggerResult{Command: "claude " + shellJoin(args)}
	if dryRun {
		return res, nil
	}

	sess, err := terminal.Start(ctx, "claude", args)
	if err != nil {
		return res, fmt.Errorf("claude interactive failed to start: %w", err)
	}
	defer sess.Close()

	output := &limitedBuffer{limit: 4096}
	go func() {
		_, _ = io.Copy(output, sess)
	}()

	done := make(chan error, 1)
	go func() {
		done <- sess.Wait()
	}()

	// Phase 1: wait for the TUI to render and settle so the submit Enter lands on
	// a ready prompt holding the prefilled message.
	if term, err := claudeAwait(ctx, sess, output, done, claudeStartupTimeout,
		func(idle, _ time.Duration) bool { return idle >= claudeStartupSettle }); term {
		return res, err
	}

	// Submit the prefilled prompt. This is the model request that anchors the 5h
	// window; the previous implementation never sent it, so the window never
	// started even though the session exited cleanly.
	if _, werr := sess.Write([]byte("\r")); werr != nil {
		return res, fmt.Errorf("claude interactive failed to submit prompt: %w: %s", werr, truncate(output.Bytes(), 300))
	}

	// Phase 2: let the turn run until its output goes quiet (bounded by a floor
	// and a hard cap), so we don't cancel the in-flight request by exiting early.
	if term, err := claudeAwait(ctx, sess, output, done, claudeTurnMaxWait,
		func(idle, elapsed time.Duration) bool {
			return elapsed >= claudeTurnMinWait && idle >= claudeTurnQuiet
		}); term {
		return res, err
	}

	// Phase 3: quit. The window is already anchored, so a messy shutdown here
	// must not fail the ping.
	_, _ = sess.Write([]byte("/exit\r"))
	select {
	case err := <-done:
		return res, claudeInteractiveErr(err, output)
	case <-ctx.Done():
		return res, claudeInteractiveCancel(ctx, sess, done, output)
	case <-time.After(claudeExitGrace):
		_ = sess.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		if accessErr := claudeSubscriptionErrorFromOutput(output.Bytes()); accessErr != nil {
			return res, accessErr
		}
		return res, nil
	}
}

// claudeAwait polls the interactive session until ready(idle, elapsed) reports
// the desired state or maxWait elapses, where idle is the time since the last
// PTY output and elapsed is the time since this phase began. It returns
// terminal=true (with an error to propagate) only if the process exits or ctx is
// cancelled first; otherwise terminal=false and the caller continues.
func claudeAwait(ctx context.Context, sess terminal.Session, output *limitedBuffer, done <-chan error, maxWait time.Duration, ready func(idle, elapsed time.Duration) bool) (bool, error) {
	start := time.Now()
	deadline := time.After(maxWait)
	ticker := time.NewTicker(claudePollInterval)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return true, claudeInteractiveErr(err, output)
		case <-ctx.Done():
			return true, claudeInteractiveCancel(ctx, sess, done, output)
		case <-deadline:
			return false, nil
		case <-ticker.C:
			changed := output.changedAt()
			if !changed.IsZero() && ready(time.Since(changed), time.Since(start)) {
				return false, nil
			}
		}
	}
}

func claudeInteractiveErr(err error, output *limitedBuffer) error {
	if accessErr := claudeSubscriptionErrorFromOutput(output.Bytes()); accessErr != nil {
		return accessErr
	}
	if err == nil {
		return nil
	}
	tail := truncate(output.Bytes(), 300)
	if tail == "" {
		return fmt.Errorf("claude interactive failed: %w", err)
	}
	return fmt.Errorf("claude interactive failed: %w: %s", err, tail)
}

// claudeSubscriptionErrorFromOutput reports the denial Claude Code printed
// itself: it exits cleanly after showing this error, so without it a ping that
// started no window would be reported as a success.
func claudeSubscriptionErrorFromOutput(raw []byte) error {
	if claudeAccessDenied(string(raw)) {
		return &ClaudeSubscriptionAccessError{}
	}
	return nil
}

func claudeInteractiveCancel(ctx context.Context, sess terminal.Session, done <-chan error, output *limitedBuffer) error {
	_ = sess.Kill()
	_ = sess.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
	if accessErr := claudeSubscriptionErrorFromOutput(output.Bytes()); accessErr != nil {
		return accessErr
	}
	tail := truncate(output.Bytes(), 300)
	if tail == "" {
		return fmt.Errorf("claude interactive cancelled: %w", ctx.Err())
	}
	return fmt.Errorf("claude interactive cancelled: %w: %s", ctx.Err(), tail)
}

func claudeInteractiveArgs(extra []string) []string {
	out := make([]string, 0, len(extra))
	for i := 0; i < len(extra); i++ {
		arg := extra[i]
		flag, inlineValue := splitFlagValue(arg)
		if claudeInteractiveUnsupportedValueArg(flag) {
			if !inlineValue && i+1 < len(extra) {
				i++
			}
			continue
		}
		if claudeInteractiveUnsupportedArg(flag) {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func splitFlagValue(arg string) (flag string, inlineValue bool) {
	if strings.HasPrefix(arg, "--") {
		if i := strings.Index(arg, "="); i > 0 {
			return arg[:i], true
		}
	}
	return arg, false
}

func claudeInteractiveUnsupportedArg(flag string) bool {
	switch flag {
	case "-p", "--print", "--bare", "--init", "--maintenance", "--include-hook-events",
		"--include-partial-messages", "--replay-user-messages", "--prompt-suggestions",
		"--no-session-persistence":
		return true
	default:
		return false
	}
}

func claudeInteractiveUnsupportedValueArg(flag string) bool {
	switch flag {
	case "--output-format", "--input-format", "--json-schema", "--max-turns",
		"--max-budget-usd", "--permission-prompt-tool", "--fallback-model":
		return true
	default:
		return false
	}
}

type limitedBuffer struct {
	mu      sync.Mutex
	limit   int
	buf     []byte
	changed time.Time // time of the last write, used to detect when output goes quiet
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if b.limit > 0 && len(b.buf) > b.limit {
		b.buf = b.buf[len(b.buf)-b.limit:]
	}
	b.changed = time.Now()
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf...)
}

// changedAt reports when output last arrived; the zero value means no output yet.
func (b *limitedBuffer) changedAt() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.changed
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
