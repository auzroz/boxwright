package config

import (
	"strings"
	"testing"
	"time"
)

func TestBindingBeyondLoopbackRequiresAToken(t *testing.T) {
	tests := []struct {
		name, addr, token string
		wantErr           bool
	}{
		{"loopback default, no token", "127.0.0.1", "", false},
		{"explicit localhost", "localhost", "", false},
		{"ipv6 loopback", "::1", "", false},
		{"all interfaces without a token", "0.0.0.0", "", true},
		{"tailnet address without a token", "100.64.21.35", "", true},
		// An unset or empty LISTEN_ADDR falls through to the loopback default,
		// not to Go's ":port" meaning of every interface -- so the safe outcome
		// is the one you get by configuring nothing.
		{"unset falls back to loopback", "", "", false},
		{"all interfaces with a token", "0.0.0.0", "s3cret", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LISTEN_ADDR", tc.addr)
			t.Setenv("BOXWRIGHT_API_TOKEN", tc.token)
			_, err := Load()
			if tc.wantErr && err == nil {
				t.Fatal("want an error: this exposes Homebox write access to the network")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "BOXWRIGHT_API_TOKEN") {
				t.Errorf("error should name the variable to set, got %q", err)
			}
		})
	}
}

// Accepting client credentials turns this into something that connects where
// a caller tells it to. Off by default, and refused outright in the one shape
// where nothing at all stands in front of it.
func TestClientHomeboxCredentials(t *testing.T) {
	tests := []struct {
		name, allow, addr, token string
		wantAllow                bool
		wantErrPart              string
	}{
		{name: "off by default", addr: "127.0.0.1", wantAllow: false},
		{name: "explicitly off", allow: "false", addr: "127.0.0.1", wantAllow: false},
		{name: "on, loopback", allow: "true", addr: "127.0.0.1", wantAllow: true},
		{name: "on, bound wide, with a token", allow: "true", addr: "0.0.0.0", token: "s3cret", wantAllow: true},
		{
			name:  "on, bound wide, no token",
			allow: "true", addr: "0.0.0.0",
			// The LISTEN_ADDR rule catches this first and names the same fix.
			wantErrPart: "BOXWRIGHT_API_TOKEN",
		},
		{name: "a value that is neither is an error, not a silent false",
			allow: "maybe", addr: "127.0.0.1", wantErrPart: "ALLOW_CLIENT_HOMEBOX"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ALLOW_CLIENT_HOMEBOX", tc.allow)
			t.Setenv("LISTEN_ADDR", tc.addr)
			t.Setenv("BOXWRIGHT_API_TOKEN", tc.token)
			c, err := Load()
			if tc.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("err = %v, want one mentioning %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.AllowClientHomebox != tc.wantAllow {
				t.Errorf("AllowClientHomebox = %v, want %v", c.AllowClientHomebox, tc.wantAllow)
			}
		})
	}
}

// The change feed is on unless someone turns it off, and its vocabulary is
// ALLOW_CLIENT_HOMEBOX's down to the near misses it refuses -- "True" included,
// which is what pins this to that switch rather than to strconv.ParseBool.
func TestHomeboxWatch(t *testing.T) {
	tests := []struct {
		name, watch string
		want        bool
		wantErrPart string
	}{
		// Nothing configured is the shape almost every deployment is in, so it
		// is the one that has to be right.
		{name: "unset means on", want: true},
		{name: "explicitly on", watch: "true", want: true},
		{name: "1 is on", watch: "1", want: true},
		{name: "yes is on", watch: "yes", want: true},
		{name: "off", watch: "false", want: false},
		{name: "0 is off", watch: "0", want: false},
		{name: "no is off", watch: "no", want: false},
		{name: "a value that is neither is an error, not a silent default",
			watch: "sometimes", wantErrPart: "HOMEBOX_WATCH"},
		// Accepting this would mean the two boolean variables in one .env
		// answer to different spellings.
		{name: "True is a startup error, not a near miss quietly taken",
			watch: "True", wantErrPart: "HOMEBOX_WATCH"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOMEBOX_WATCH", tc.watch)
			c, err := Load()
			if tc.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("err = %v, want one naming %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.HomeboxWatch != tc.want {
				t.Errorf("HomeboxWatch = %v, want %v", c.HomeboxWatch, tc.want)
			}
		})
	}
}

// The TTL still bounds how stale the index can get, with a change feed or
// without one: the feed has no resume cursor, so a socket that dies quietly
// leaves a cache that would otherwise report itself fresh forever.
func TestBoxCacheTTL(t *testing.T) {
	tests := []struct {
		name, ttl   string
		want        time.Duration
		wantErrPart string
	}{
		{name: "unset is five minutes", want: 5 * time.Minute},
		{name: "an override is taken as written", ttl: "90s", want: 90 * time.Second},
		// A cache TTL is exactly where someone writes seconds as a bare number.
		{name: "a bare number names no unit", ttl: "300", wantErrPart: "BOX_CACHE_TTL"},
		{name: "a unit spelled out", ttl: "5 minutes", wantErrPart: "BOX_CACHE_TTL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BOX_CACHE_TTL", tc.ttl)
			c, err := Load()
			if tc.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("err = %v, want one naming %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.BoxCacheTTL != tc.want {
				t.Errorf("BoxCacheTTL = %v, want %v", c.BoxCacheTTL, tc.want)
			}
		})
	}
}

// Each provider has its own idea of what the user must supply: the two that
// front somebody else's endpoint need to be told where and what, while the
// Anthropic provider knows its own host and model and only needs a credential.
func TestAIProviderRequirements(t *testing.T) {
	tests := []struct {
		name                     string
		provider, base, key, mod string
		wantErrContains          string
	}{
		{name: "none needs nothing", provider: "none"},
		{name: "unset defaults to none"},
		{name: "openai fully configured", provider: "openai", base: "https://api.openai.com/v1", mod: "gpt-5-mini"},
		{name: "openai without a base URL", provider: "openai", mod: "gpt-5-mini", wantErrContains: "AI_BASE_URL"},
		{name: "openai without a model", provider: "openai", base: "https://api.openai.com/v1", wantErrContains: "AI_MODEL"},
		{name: "ollama without a base URL", provider: "ollama", mod: "gemma3:4b", wantErrContains: "AI_BASE_URL"},
		{name: "ollama without a model", provider: "ollama", base: "http://ollama:11434", wantErrContains: "AI_MODEL"},

		// Both the host and the model default for this provider, so a key alone
		// is a complete configuration.
		{name: "anthropic with only a key", provider: "anthropic", key: "sk-ant-x"},
		{name: "anthropic without a key", provider: "anthropic", wantErrContains: "AI_API_KEY"},
		// A gateway can hold the credential itself; requiring a key we would
		// never use would block that setup for no gain.
		{name: "anthropic behind a gateway", provider: "anthropic", base: "http://llm-gateway:4000"},

		// The development-only CLI provider carries its own credentials (an
		// interactive `claude` login) and its own endpoint, so demanding either
		// would be demanding something the user has no way to supply.
		{name: "claude-code needs nothing", provider: "claude-code"},
		{name: "claude-code ignores a stray key", provider: "claude-code", key: "sk-ant-x", mod: "claude-haiku-4-5"},

		// "claude" is not "claude-code": the near-miss must still be rejected
		// rather than silently starting the wrong provider.
		{name: "unknown provider", provider: "claude", wantErrContains: "AI_PROVIDER"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AI_PROVIDER", tc.provider)
			t.Setenv("AI_BASE_URL", tc.base)
			t.Setenv("AI_API_KEY", tc.key)
			t.Setenv("AI_MODEL", tc.mod)
			_, err := Load()
			switch {
			case tc.wantErrContains == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErrContains != "" && err == nil:
				t.Fatalf("want an error naming %s", tc.wantErrContains)
			case tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains):
				t.Errorf("error should name %s, got %q", tc.wantErrContains, err)
			}
		})
	}
}

// A config dump must never carry the Homebox key into logs.
func TestLogValueRedactsSecrets(t *testing.T) {
	c := Config{HomeboxAPIKey: "hb_realkey", AIAPIKey: "sk-real", APIToken: "s3cret"}
	got := c.LogValue().String()
	for _, secret := range []string{"hb_realkey", "sk-real", "s3cret"} {
		if strings.Contains(got, secret) {
			t.Errorf("LogValue leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "(set)") {
		t.Errorf("LogValue should still report presence, got %s", got)
	}
}
