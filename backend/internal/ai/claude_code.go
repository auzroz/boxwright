package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"boxwright/internal/placement"
)

// DEVELOPMENT ONLY. This provider shells out to the Claude Code CLI instead of
// calling an inference API. It exists for exactly one job: letting the owner
// evaluate identification quality and build a golden corpus against an existing
// Claude Code subscription, before opening a metered API relationship. It is
// not a deployment path, and three things make that permanent:
//
//  1. It costs MORE per photo than the API it stands in for -- measured at 16x
//     on a real photo (Opus 5: $0.18 here vs ~$0.011 direct; Haiku: $0.032 vs
//     ~$0.002). Every invocation re-sends the Claude Code system prompt and
//     tool definitions, ~37k cache-read plus ~12k cache-creation tokens of
//     harness that has nothing to do with the photo.
//  2. It needs the `claude` binary installed AND interactively authenticated on
//     the machine running the backend. A container image does not have that,
//     and a headless server only gets it by someone sitting down at it.
//  3. Claude Code is a developer tool, not an inference API. Wiring it in as an
//     application's inference backend is outside what it is for. Fine on your
//     own laptop while you are grading answers; AI_PROVIDER=anthropic is the
//     answer for anything real.
//
// Everything here therefore optimises for an evaluator watching it run --
// visible cost, actionable failures -- rather than for throughput.
const (
	// claudeCodeDefaultCLI is resolved through PATH. CLAUDE_CLI_PATH overrides
	// it, because the CLI commonly lives in ~/.local/bin, which a launchd or
	// systemd service does not inherit.
	claudeCodeDefaultCLI = "claude"

	// claudeCodeDefaultPermissionMode: see the comment on commandArgs. The
	// measured invocation used acceptEdits; this deliberately does not.
	claudeCodeDefaultPermissionMode = "default"

	// claudeCodeWaitDelay bounds how long Wait may block after the context
	// kills the CLI. Stdout is a buffer, so os/exec pipes it through a copying
	// goroutine, and a grandchild that inherits the write end would otherwise
	// hold Wait open indefinitely.
	claudeCodeWaitDelay = 5 * time.Second

	// claudeCodeSnippetBytes bounds CLI output quoted into an error. Enough to
	// name the failure, short enough not to paste a transcript into a log line.
	claudeCodeSnippetBytes = 400
)

type claudeCodeProvider struct {
	cliPath        string // binary name or absolute path; resolved with LookPath
	model          string // empty means "whatever the CLI is configured to use"
	permissionMode string
	// tempDir is the root the photo is staged under. Empty means os.TempDir();
	// tests point it at a directory they can prove is empty afterwards, which
	// is also why staging is not hardcoded to os.TempDir().
	tempDir string
	log     *slog.Logger
}

// newClaudeCode builds the provider. The two knobs are read here rather than in
// config.Config because ai.New's signature carries only the four AI_* values
// every provider shares, and neither knob is meaningful to any other provider.
func newClaudeCode(model string) *claudeCodeProvider {
	p := &claudeCodeProvider{
		cliPath:        os.Getenv("CLAUDE_CLI_PATH"),
		model:          strings.TrimSpace(model),
		permissionMode: os.Getenv("CLAUDE_CODE_PERMISSION_MODE"),
	}
	if p.cliPath == "" {
		p.cliPath = claudeCodeDefaultCLI
	}
	if p.permissionMode == "" {
		p.permissionMode = claudeCodeDefaultPermissionMode
	}
	return p
}

func (p *claudeCodeProvider) logger() *slog.Logger {
	if p.log != nil {
		return p.log
	}
	return slog.Default()
}

// claudeCodeEnvelope is the JSON object `--output-format json` prints on
// stdout. Only the fields acted on are declared; the CLI adds more (usage,
// modelUsage, terminal_reason) and is free to keep adding them.
type claudeCodeEnvelope struct {
	Result        string  `json:"result"` // the model's reply, often inside a ```json fence
	IsError       bool    `json:"is_error"`
	TotalCostUSD  float64 `json:"total_cost_usd"`
	DurationAPIMs int64   `json:"duration_api_ms"`
	SessionID     string  `json:"session_id"`
	Subtype       string  `json:"subtype"`
	// PermissionDenials is the difference between "the model failed" and "we
	// asked it to do something we had not allowed", which are fixed in
	// completely different places.
	PermissionDenials []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
}

func (p *claudeCodeProvider) Identify(ctx context.Context, image []byte, mimeType string, categories []placement.Category) ([]placement.ItemDraft, error) {
	// The CLI reads images from disk, so the photo has to touch the filesystem.
	// It is a picture of someone's belongings; see writeTempImage for what that
	// costs us. cleanup is deferred rather than called at each return so no
	// path out of this function -- including a panic -- leaves it behind.
	imagePath, cleanup, err := writeTempImage(p.tempDir, image, mimeType)
	if err != nil {
		return nil, fmt.Errorf("claude-code: staging the photo: %w", err)
	}
	defer cleanup()

	bin, err := exec.LookPath(p.cliPath)
	if err != nil {
		return nil, fmt.Errorf(
			"claude-code: cannot run %q (%v): install the Claude Code CLI and sign in once with `claude`, "+
				"or set CLAUDE_CLI_PATH to its full path (it is often ~/.local/bin/claude, which services do not inherit); %w",
			p.cliPath, err, ErrNoProvider)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, p.commandArgs(claudeCodePrompt(imagePath, categories))...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// No stdin: -p takes the prompt as an argument, and a CLI that decides to
	// ask a question should hit EOF rather than wait for a human who is not there.
	cmd.Stdin = nil
	cmd.WaitDelay = claudeCodeWaitDelay
	// cmd.Dir is deliberately left inherited. Running each call in its own
	// scratch directory would look tidier, but the working directory is part of
	// the harness prompt prefix, so changing it every call would defeat the
	// prompt cache and turn ~37k cached tokens per photo into billed ones.

	start := time.Now()
	runErr := cmd.Run()
	wall := time.Since(start)

	// Checked before runErr: a cancelled context surfaces as a generic "signal:
	// killed" exit, which would otherwise be reported as a CLI malfunction.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf(
			"claude-code: the CLI did not finish in time (%s, %v); a Claude Code run costs several seconds "+
				"of harness startup before the model sees the photo, so allow a generous request timeout; %w",
			wall.Round(time.Millisecond), ctxErr, ErrNoProvider)
	}
	if runErr != nil {
		return nil, fmt.Errorf(
			"claude-code: the CLI exited with an error (%v)%s: %s; %w",
			runErr, claudeCodeAuthHint(stderr.String(), stdout.String()), claudeCodeSnippet(stderr.String()), ErrNoProvider)
	}

	raw := bytes.TrimSpace(stdout.Bytes())
	if len(raw) == 0 {
		return nil, fmt.Errorf(
			"claude-code: the CLI exited successfully but printed nothing%s; %w",
			claudeCodeAuthHint(stderr.String(), ""), ErrNoProvider)
	}
	var env claudeCodeEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Not the model's JSON -- the CLI's. A version whose --output-format
		// json envelope changed shape lands here, and the raw snippet is the
		// only thing that makes that diagnosable.
		return nil, fmt.Errorf(
			"claude-code: the CLI's output is not the expected JSON envelope (%v): %s; %w",
			err, claudeCodeSnippet(string(raw)), ErrNoProvider)
	}

	// Info, not Debug: the whole point of this provider is evaluating quality
	// against a subscription, and the owner should be able to watch what that
	// evaluation is costing while it happens rather than reconstruct it later.
	// Logged before the failure branches because a failed run still bills.
	// Never the image bytes, never the staged path.
	p.logger().Info("claude-code identify",
		"costUSD", env.TotalCostUSD,
		"apiMs", env.DurationAPIMs,
		"wallMs", wall.Milliseconds(),
		"model", p.modelLabel(),
		"sessionId", env.SessionID)

	if env.IsError {
		return nil, fmt.Errorf(
			"claude-code: the CLI reported a failed turn (%s)%s: %s; %w",
			orUnknown(env.Subtype), p.denialHint(env), claudeCodeSnippet(env.Result), ErrNoProvider)
	}
	if strings.TrimSpace(env.Result) == "" {
		return nil, fmt.Errorf(
			"claude-code: the CLI returned an empty result%s; %w", p.denialHint(env), ErrNoProvider)
	}

	drafts, err := parseDrafts(env.Result)
	if err != nil {
		// Wrapped alongside ErrNoProvider: an unusable draft means "no draft
		// available", and the app already knows how to offer manual entry for
		// that. Failing the capture instead would break "rules first".
		return nil, fmt.Errorf("claude-code: %w; %w", err, ErrNoProvider)
	}
	return drafts, nil
}

// commandArgs assembles the CLI invocation.
//
// On --permission-mode: the measured invocation used acceptEdits, and this does
// not. acceptEdits pre-approves file *writes* on the owner's machine, and this
// provider has exactly one filesystem need -- reading back a single JPEG we
// just wrote -- so granting write authority buys nothing and hands an
// unattended, model-driven process the ability to change files. `default` is
// the most restrictive mode that still runs the tool we do need: --allowedTools
// Read pre-approves the read, and anything outside that allowlist has no
// interactive session to approve it, so the restrictive mode degrades to a
// denial rather than to a prompt nobody will answer. If a future CLI version
// disagrees, the denial is reported by name (see denialHint) and
// CLAUDE_CODE_PERMISSION_MODE restores the measured configuration without a
// code change.
func (p *claudeCodeProvider) commandArgs(prompt string) []string {
	args := []string{
		"-p", prompt,
		"--output-format", "json",
		"--allowedTools", "Read",
		"--permission-mode", p.permissionMode,
	}
	// An unset AI_MODEL means "whatever this machine's CLI is configured to
	// use". Passing an empty --model would be an error, and picking a default
	// here would silently overrule the owner's own choice.
	if p.model != "" {
		args = append(args, "--model", p.model)
	}
	return args
}

func (p *claudeCodeProvider) modelLabel() string {
	if p.model == "" {
		return "(cli default)"
	}
	return p.model
}

// denialHint turns "no answer" into "you did not allow the tool it needed",
// naming the escape hatch, because the two failures look identical otherwise.
func (p *claudeCodeProvider) denialHint(env claudeCodeEnvelope) string {
	if len(env.PermissionDenials) == 0 {
		return ""
	}
	names := make([]string, 0, len(env.PermissionDenials))
	for _, d := range env.PermissionDenials {
		names = append(names, orUnknown(d.ToolName))
	}
	return fmt.Sprintf(
		" (the CLI was denied %s under --permission-mode %s; if reading the photo was denied, "+
			"set CLAUDE_CODE_PERMISSION_MODE=acceptEdits)",
		strings.Join(names, ", "), p.permissionMode)
}

// claudeCodeAuthHint recognises the failure an evaluator hits first: the CLI is
// installed, so LookPath succeeded, but nobody ever signed in on this machine.
// The generic "exit status 1" gives no clue what to do about that.
func claudeCodeAuthHint(streams ...string) string {
	for _, s := range streams {
		l := strings.ToLower(s)
		for _, marker := range []string{
			"/login", "not logged in", "log in", "invalid api key",
			"authentication", "unauthorized", "oauth", "credentials",
		} {
			if strings.Contains(l, marker) {
				return " (the CLI is installed but not authenticated on this machine: " +
					"run `claude` once interactively and sign in)"
			}
		}
	}
	return ""
}

// claudeCodePrompt reuses identifyPrompt verbatim -- the vocabulary handling
// and the "propose a new category" instruction are the same contract for every
// provider -- and appends only what is specific to a CLI that reads from disk.
//
// The path goes on its own trailing line so it is unambiguous to the model, and
// the prompt travels as a single argv element, so nothing in it is ever
// interpreted by a shell.
func claudeCodePrompt(imagePath string, categories []placement.Category) string {
	var b strings.Builder
	b.WriteString(identifyPrompt(categories))
	b.WriteString("\n\nRead the image file named below with the Read tool, then answer for the item it shows. " +
		"Output only the JSON object: no preamble, no explanation, no follow-up question.\n" +
		"Image file: ")
	b.WriteString(imagePath)
	return b.String()
}

// writeTempImage stages the photo where the CLI can read it, and returns the
// cleanup that must run on every path out of Identify.
//
// The bytes are a photograph of the inside of someone's home. The shared temp
// directory is world-readable on a typical machine, and this file lives long
// enough for a whole model round trip, so it goes in a 0700 directory of its
// own with 0600 permissions: no other account can list it, traverse to it, or
// open it. Removing the directory (not just the file) also means a CLI that
// writes a sibling file cannot leave that behind either.
func writeTempImage(root string, image []byte, mimeType string) (string, func(), error) {
	noop := func() {}
	dir, err := os.MkdirTemp(root, "boxwright-photo-") // MkdirTemp creates 0700
	if err != nil {
		return "", noop, fmt.Errorf("creating a private staging directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	path := filepath.Join(dir, "item"+imageExtension(mimeType))
	if err := os.WriteFile(path, image, 0o600); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("writing the photo to %s: %w", dir, err)
	}
	return path, cleanup, nil
}

// imageExtension maps the upload's Content-Type to a file extension, which is
// how a tool reading from disk decides the file is an image at all. Unknown
// types fall back to .jpg -- the same assumption the upload handler makes for a
// missing Content-Type, and a guess that has a chance of working where a
// refusal has none.
func imageExtension(mimeType string) string {
	m := strings.ToLower(strings.TrimSpace(mimeType))
	if i := strings.Index(m, ";"); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch m {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/heic":
		return ".heic"
	case "image/heif":
		return ".heif"
	default: // image/jpeg, image/jpg, empty, anything unrecognised
		return ".jpg"
	}
}

// claudeCodeSnippet bounds CLI output before it reaches an error message.
func claudeCodeSnippet(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no output)"
	}
	if len(s) > claudeCodeSnippetBytes {
		// ToValidUTF8 because the cut can land mid-rune.
		s = strings.ToValidUTF8(s[:claudeCodeSnippetBytes], "") + "..."
	}
	return s
}
