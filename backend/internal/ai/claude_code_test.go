package ai

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// No test in this file may ever invoke the real `claude` binary: it spends real
// money (measured $0.18 a call on Opus 5) and needs an interactive login this
// machine may not have. Every case runs a shell script standing in for the CLI,
// which is also the only way to assert what argv we build -- the part a live
// run would never show us.

// fakeCLI is a stand-in `claude` that records its argv, copies the photo it was
// pointed at (proving the file existed, and with which bytes, at the moment of
// the call), then prints canned output and exits with a canned status.
type fakeCLI struct {
	path      string
	argvFile  string
	imageCopy string
}

func newFakeCLI(t *testing.T, stdout, stderr string, exitCode int) *fakeCLI {
	t.Helper()
	dir := t.TempDir()
	f := &fakeCLI{
		path:      filepath.Join(dir, "claude"),
		argvFile:  filepath.Join(dir, "argv"),
		imageCopy: filepath.Join(dir, "image-copy"),
	}
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("writing fake CLI fixture: %v", err)
		}
		return p
	}
	outFile, errFile := write("stdout", stdout), write("stderr", stderr)

	// Paths come from t.TempDir(), so single quotes are sufficient quoting.
	// Args are separated by \036 rather than newline: the prompt is multi-line,
	// so a newline-separated record would split it into fragments.
	script := "#!/bin/sh\n" +
		"printf '%s\\036' \"$@\" > '" + f.argvFile + "'\n" +
		// The prompt's last line is "Image file: <path>"; recover it and copy
		// the staged photo before returning, so the test can inspect a file
		// that is deliberately gone by the time Identify returns.
		"img=$(tr '\\036' '\\n' < '" + f.argvFile + "' | sed -n 's/^Image file: //p')\n" +
		"[ -n \"$img\" ] && cp \"$img\" '" + f.imageCopy + "'\n" +
		"cat '" + outFile + "'\n" +
		"cat '" + errFile + "' >&2\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(f.path, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake CLI: %v", err)
	}
	return f
}

func (f *fakeCLI) argv(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(f.argvFile)
	if err != nil {
		t.Fatalf("fake CLI recorded no argv (was it run?): %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x1e"), "\x1e")
}

// newTestProvider wires the provider to a fake binary and to a staging root the
// test owns, so leftover photos are observable.
func newTestProvider(t *testing.T, cliPath, model string) (*claudeCodeProvider, string) {
	t.Helper()
	root := t.TempDir()
	return &claudeCodeProvider{
		cliPath:        cliPath,
		model:          model,
		permissionMode: claudeCodeDefaultPermissionMode,
		tempDir:        root,
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, root
}

// assertNoPhotoLeftBehind is asserted on EVERY path, success and failure alike:
// a photo of someone's home outliving the request it arrived in is the failure
// mode that does not announce itself.
func assertNoPhotoLeftBehind(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading staging root: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("staged photo left behind in %s: %v", root, names)
	}
}

const fence = "```"

// claudeCodeEnvelopeJSON builds the CLI's JSON envelope around a model reply.
func claudeCodeEnvelopeJSON(t *testing.T, result string) string {
	t.Helper()
	return `{"type":"result","subtype":"success","is_error":false,` +
		`"total_cost_usd":0.1802,"duration_api_ms":8100,"session_id":"sess_1",` +
		`"result":` + quoteJSON(result) + `}`
}

func quoteJSON(s string) string {
	r := strings.NewReplacer("\\", `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

const claudeCodeDraftJSON = `{"name":"cordless drill","category":"tools","sizeBucket":"M",` +
	`"fragile":false,"weightClass":"medium","notes":"DeWalt DCD777","confidence":0.9}`

// The happy path, and with it the whole invocation contract: the flags, the
// prompt, and the staged photo the CLI is pointed at.
func TestClaudeCodeIdentify(t *testing.T) {
	f := newFakeCLI(t, claudeCodeEnvelopeJSON(t, claudeCodeDraftJSON), "", 0)
	p, root := newTestProvider(t, f.path, "claude-haiku-4-5")

	image := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'j', 'p', 'g'}
	drafts, err := p.Identify(context.Background(), image, "image/jpeg", liveVocabulary())
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}

	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1", len(drafts))
	}
	draft := drafts[0]
	if draft.Name != "cordless drill" || draft.Category != "tools" {
		t.Errorf("draft = %+v, want the CLI's reply parsed and normalized", draft)
	}
	if draft.Confidence != 0.9 {
		t.Errorf("confidence = %v, want 0.9", draft.Confidence)
	}

	// The CLI could only have written this by reading the file we staged.
	copied, err := os.ReadFile(f.imageCopy)
	if err != nil {
		t.Fatalf("the CLI could not read the staged photo: %v", err)
	}
	if string(copied) != string(image) {
		t.Errorf("staged photo = %q, want the uploaded bytes verbatim", copied)
	}
	assertNoPhotoLeftBehind(t, root)
}

func TestClaudeCodeCommandArgs(t *testing.T) {
	f := newFakeCLI(t, claudeCodeEnvelopeJSON(t, claudeCodeDraftJSON), "", 0)
	p, _ := newTestProvider(t, f.path, "claude-haiku-4-5")
	if _, err := p.Identify(context.Background(), []byte("jpeg"), "image/png", liveVocabulary()); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	argv := f.argv(t)

	for _, pair := range [][2]string{
		{"--output-format", "json"},
		{"--allowedTools", "Read"},
		{"--model", "claude-haiku-4-5"},
		// The most restrictive mode that still runs Read. acceptEdits would
		// pre-approve file WRITES for a job whose only filesystem need is
		// reading back one JPEG we just wrote.
		{"--permission-mode", "default"},
	} {
		if !hasFlagValue(argv, pair[0], pair[1]) {
			t.Errorf("argv is missing %s %s: %v", pair[0], pair[1], argv)
		}
	}
	if hasFlagValue(argv, "--permission-mode", "acceptEdits") ||
		hasFlagValue(argv, "--permission-mode", "bypassPermissions") {
		t.Errorf("argv grants write or blanket permissions to a read-only job: %v", argv)
	}
	if argv[0] != "-p" {
		t.Errorf("argv[0] = %q, want -p (non-interactive print mode): %v", argv[0], argv)
	}

	// The prompt must be identifyPrompt's, not a restatement of it, and must
	// carry the live vocabulary through to the model.
	prompt := argv[1]
	if !strings.Contains(prompt, "micro-masterpieces") ||
		!strings.Contains(strings.ToLower(prompt), "not exhaustive") {
		t.Errorf("prompt does not reuse identifyPrompt with the supplied vocabulary:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Image file: ") {
		t.Errorf("prompt never tells the model which file to read:\n%s", prompt)
	}
	// The extension follows the upload's MIME type: a tool reading from disk
	// uses it to decide the file is an image at all.
	if !strings.Contains(prompt, ".png") {
		t.Errorf("staged an image/png upload without a .png extension:\n%s", prompt)
	}
}

// An unset AI_MODEL must defer to whatever the machine's CLI is configured to
// use, rather than us overruling the owner's own choice with a default.
func TestClaudeCodeOmitsModelFlagWhenUnset(t *testing.T) {
	f := newFakeCLI(t, claudeCodeEnvelopeJSON(t, claudeCodeDraftJSON), "", 0)
	p, _ := newTestProvider(t, f.path, "")
	if _, err := p.Identify(context.Background(), []byte("jpeg"), "image/jpeg", nil); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	for _, arg := range f.argv(t) {
		if arg == "--model" {
			t.Errorf("passed --model with no AI_MODEL set: %v", f.argv(t))
		}
	}
}

// Claude Code habitually fences its JSON. parseDraft already strips fences;
// this proves the envelope's result reaches it rather than being pre-chewed.
func TestClaudeCodeFencedResult(t *testing.T) {
	fenced := fence + "json\n" + claudeCodeDraftJSON + "\n" + fence
	f := newFakeCLI(t, claudeCodeEnvelopeJSON(t, fenced), "", 0)
	p, root := newTestProvider(t, f.path, "")

	drafts, err := p.Identify(context.Background(), []byte("jpeg"), "image/jpeg", nil)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(drafts) != 1 || drafts[0].Name != "cordless drill" {
		t.Errorf("drafts = %+v, want the fenced JSON parsed into one draft", drafts)
	}
	assertNoPhotoLeftBehind(t, root)
}

// Every way this provider can fail to produce a draft. All of them must wrap
// ErrNoProvider, because "no draft available" is exactly the condition
// internal/api already turns into the 422 manual-entry response -- the app has
// to stay usable when the CLI is missing, unauthenticated, or confused.
func TestClaudeCodeFailureModes(t *testing.T) {
	tests := []struct {
		name           string
		stdout, stderr string
		exitCode       int
		wantContains   string
	}{
		{
			name:         "envelope reports a failed turn",
			stdout:       `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"tool loop failed"}`,
			wantContains: "failed turn",
		},
		{
			name:         "permission denial is named, not guessed at",
			stdout:       `{"is_error":true,"result":"","permission_denials":[{"tool_name":"Read"}]}`,
			wantContains: "denied Read",
		},
		{
			name:         "non-zero exit",
			stderr:       "unexpected crash in the harness",
			exitCode:     3,
			wantContains: "unexpected crash",
		},
		{
			name:         "installed but never signed in",
			stderr:       "Invalid API key · Please run /login",
			exitCode:     1,
			wantContains: "not authenticated",
		},
		{
			name:         "output is not the JSON envelope",
			stdout:       "Welcome to Claude Code!",
			wantContains: "not the expected JSON envelope",
		},
		{
			name:         "no output at all",
			wantContains: "printed nothing",
		},
		{
			name:         "empty result",
			stdout:       `{"is_error":false,"result":"   "}`,
			wantContains: "empty result",
		},
		{
			name:         "result is prose, not a draft",
			stdout:       `{"is_error":false,"result":"I think that is a drill."}`,
			wantContains: "unparseable draft",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCLI(t, tc.stdout, tc.stderr, tc.exitCode)
			p, root := newTestProvider(t, f.path, "")

			_, err := p.Identify(context.Background(), []byte("jpeg"), "image/jpeg", liveVocabulary())
			if err == nil {
				t.Fatal("want an error")
			}
			if !errors.Is(err, ErrNoProvider) {
				t.Errorf("error does not wrap ErrNoProvider, so the app 502s instead of "+
					"offering manual entry: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantContains) {
				t.Errorf("error should mention %q, got %q", tc.wantContains, err)
			}
			assertNoPhotoLeftBehind(t, root)
		})
	}
}

// A missing binary is the first thing an evaluator hits, and "exec: not found"
// alone does not say what to install or which variable overrides the path.
func TestClaudeCodeMissingBinary(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-claude")
	p, root := newTestProvider(t, missing, "")

	_, err := p.Identify(context.Background(), []byte("jpeg"), "image/jpeg", nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrNoProvider) {
		t.Errorf("error does not wrap ErrNoProvider: %v", err)
	}
	if !strings.Contains(err.Error(), "CLAUDE_CLI_PATH") {
		t.Errorf("error should name the override, got %q", err)
	}
	assertNoPhotoLeftBehind(t, root)
}

// The caller's context must actually stop the CLI: /identify holds an HTTP
// request open, and a harness that ignores cancellation would keep spending
// after the phone has walked away.
func TestClaudeCodeContextTimeout(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	// exec, so the signal reaches sleep rather than a shell that is waiting on it.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatalf("writing fake CLI: %v", err)
	}
	p, root := newTestProvider(t, script, "")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := p.Identify(ctx, []byte("jpeg"), "image/jpeg", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrNoProvider) {
		t.Errorf("a timeout should still route to manual entry: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "did not finish in time") {
		t.Errorf("error should name the timeout, got %q", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %s: the CLI was not killed when the context expired", elapsed)
	}
	assertNoPhotoLeftBehind(t, root)
}

// The photo is a picture of the inside of someone's home. It must not be
// readable by other accounts on the machine while it waits for the model.
func TestStagedPhotoIsPrivateThenRemoved(t *testing.T) {
	root := t.TempDir()
	image := []byte("not really a jpeg")

	path, cleanup, err := writeTempImage(root, image, "image/png")
	if err != nil {
		t.Fatalf("writeTempImage: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat photo: %v", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("photo mode %v is readable beyond its owner", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat staging dir: %v", err)
	}
	if di.Mode().Perm()&0o077 != 0 {
		t.Errorf("staging dir mode %v lets other accounts traverse to the photo", di.Mode().Perm())
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(image) {
		t.Errorf("staged bytes = %q (err %v), want the upload verbatim", got, err)
	}

	cleanup()
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("staging directory survived cleanup: %v", err)
	}
	assertNoPhotoLeftBehind(t, root)
}

func TestImageExtension(t *testing.T) {
	tests := []struct{ mime, want string }{
		{"image/jpeg", ".jpg"},
		{"image/jpg", ".jpg"},
		{"image/png", ".png"},
		// Multipart uploads carry parameters; the type is still png.
		{"image/png; charset=binary", ".png"},
		{"IMAGE/PNG", ".png"},
		{"image/webp", ".webp"},
		{"image/gif", ".gif"},
		{"image/heic", ".heic"},
		// Unknown and empty both fall back to the upload handler's assumption.
		{"application/octet-stream", ".jpg"},
		{"", ".jpg"},
	}
	for _, tc := range tests {
		if got := imageExtension(tc.mime); got != tc.want {
			t.Errorf("imageExtension(%q) = %q, want %q", tc.mime, got, tc.want)
		}
	}
}

// The factory must accept the provider with neither a base URL nor a key: the
// CLI carries both itself, so requiring either would be requiring something the
// user cannot supply.
func TestNewClaudeCodeNeedsNoCredentials(t *testing.T) {
	// Pin the environment this test reads. Without it the case under test is
	// whatever the developer happens to have exported -- and CLAUDE_CLI_PATH
	// is exactly what someone running the provider for real will have set,
	// so the suite would go red on the machines most likely to run it.
	t.Setenv("CLAUDE_CLI_PATH", "")
	t.Setenv("CLAUDE_CODE_PERMISSION_MODE", "")

	p, err := New("claude-code", "", "", "")
	if err != nil {
		t.Fatalf("New(claude-code): %v", err)
	}
	cc, ok := p.(*claudeCodeProvider)
	if !ok {
		t.Fatalf("New returned %T, want *claudeCodeProvider", p)
	}
	if cc.cliPath != claudeCodeDefaultCLI {
		t.Errorf("cliPath = %q, want %q resolved through PATH", cc.cliPath, claudeCodeDefaultCLI)
	}
	if cc.permissionMode != claudeCodeDefaultPermissionMode {
		t.Errorf("permissionMode = %q, want %q", cc.permissionMode, claudeCodeDefaultPermissionMode)
	}
}

// Both knobs are read from the environment, because ai.New's signature carries
// only the AI_* values every provider shares.
func TestClaudeCodeEnvOverrides(t *testing.T) {
	t.Setenv("CLAUDE_CLI_PATH", "/opt/homebrew/bin/claude")
	t.Setenv("CLAUDE_CODE_PERMISSION_MODE", "acceptEdits")

	cc := newClaudeCode("claude-opus-5")
	if cc.cliPath != "/opt/homebrew/bin/claude" {
		t.Errorf("cliPath = %q, want the CLAUDE_CLI_PATH override", cc.cliPath)
	}
	if cc.permissionMode != "acceptEdits" {
		t.Errorf("permissionMode = %q, want the override", cc.permissionMode)
	}
	if !hasFlagValue(cc.commandArgs("prompt"), "--permission-mode", "acceptEdits") {
		t.Errorf("the override never reaches argv: %v", cc.commandArgs("prompt"))
	}
}

func hasFlagValue(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}
