// Package config loads backend configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"boxwright/internal/ai"
)

// Config holds all runtime configuration. See README.md for the reference table.
type Config struct {
	Port string
	// ListenAddr is the interface to bind. It defaults to loopback because
	// this service holds a Homebox API key and will happily write to someone's
	// inventory; binding it to every interface should be a deliberate act, not
	// the default you get by not thinking about it.
	ListenAddr string
	// APIToken, when set, is required as a Bearer token on every /api request.
	// Binding off loopback without one is refused.
	APIToken       string
	HomeboxBaseURL string // includes /api, e.g. http://homebox:7745/api
	HomeboxAPIKey  string // hb_... static key or session bearer token

	// AllowClientHomebox lets a request carry its own Homebox URL and token,
	// so one backend can serve people who each have their own instance -- what
	// a signed app build needs, since its users' items must not land in the
	// operator's inventory.
	//
	// Off by default, and it should stay off for a self-hosted single-user
	// deployment. Turning it on means an authenticated caller can name any
	// address this server can reach, so the /api shared token stops being just
	// an access control and becomes the boundary of that too.
	AllowClientHomebox bool

	// HomeboxWatch subscribes to Homebox's change feed, so an edit made
	// elsewhere -- the web UI on a laptop -- reaches the box index without
	// waiting out BoxCacheTTL.
	//
	// On by default because there is nothing to weigh: a Homebox too old to
	// have the socket, a refused key, or a storage unit with no signal all
	// degrade to exactly the TTL-plus-patch behaviour that came before it.
	// Turning it off means this backend holds no long-lived outbound
	// connection at all.
	HomeboxWatch bool

	// AIProvider is "openai", "ollama", "anthropic", "claude-code", or "none".
	// claude-code is a development-only evaluation path (it shells out to the
	// Claude Code CLI); see internal/ai/claude_code.go.
	AIProvider string
	AIBaseURL  string
	AIAPIKey   string
	AIModel    string
	// AIEffort and AIThinking tune how the model is asked, where it takes
	// them (Anthropic): effort low..max, thinking auto|adaptive. Empty keeps
	// the defaults, which cmd/identeval is for choosing.
	AIEffort   string
	AIThinking string

	BoxCacheTTL time.Duration
}

// Load reads configuration from the environment. It returns an error for
// invalid values but tolerates a missing Homebox key so the server can start
// in degraded mode (health endpoint up, clear errors elsewhere).
func Load() (Config, error) {
	c := Config{
		Port:           getenv("PORT", "8080"),
		ListenAddr:     getenv("LISTEN_ADDR", "127.0.0.1"),
		APIToken:       os.Getenv("BOXWRIGHT_API_TOKEN"),
		HomeboxBaseURL: getenv("HOMEBOX_BASE_URL", "http://homebox:7745/api"),
		HomeboxAPIKey:  os.Getenv("HOMEBOX_API_KEY"),
		AIProvider:     getenv("AI_PROVIDER", "none"),
		AIBaseURL:      os.Getenv("AI_BASE_URL"),
		AIAPIKey:       os.Getenv("AI_API_KEY"),
		AIModel:        os.Getenv("AI_MODEL"),
		AIEffort:       os.Getenv("AI_EFFORT"),
		AIThinking:     os.Getenv("AI_THINKING"),
	}
	if err := ai.ValidateTuning(c.AIEffort, c.AIThinking); err != nil {
		return c, fmt.Errorf("AI_EFFORT / AI_THINKING: %w", err)
	}

	allow := getenv("ALLOW_CLIENT_HOMEBOX", "false")
	switch allow {
	case "true", "1", "yes":
		c.AllowClientHomebox = true
	case "false", "0", "no":
	default:
		return c, fmt.Errorf("ALLOW_CLIENT_HOMEBOX must be true or false; got %q", allow)
	}

	// Same vocabulary as ALLOW_CLIENT_HOMEBOX above, deliberately: two boolean
	// variables in one .env that disagree about whether "yes" is a word is a
	// trap. strconv.ParseBool would take "T" and reject "yes", which is that
	// disagreement. Unreadable is an error rather than a default, because a
	// typo that silently disabled the feed is indistinguishable from a Homebox
	// that never had one.
	watch := getenv("HOMEBOX_WATCH", "true")
	switch watch {
	case "true", "1", "yes":
		c.HomeboxWatch = true
	case "false", "0", "no":
	default:
		return c, fmt.Errorf("HOMEBOX_WATCH must be true or false; got %q", watch)
	}

	// A phone cannot reach loopback, so real use means binding wider -- but then
	// anything on the network can write to Homebox through this service. Refuse
	// rather than quietly expose it.
	if !isLoopback(c.ListenAddr) && c.APIToken == "" {
		return c, fmt.Errorf(
			"LISTEN_ADDR=%s exposes this service beyond loopback, so BOXWRIGHT_API_TOKEN must be set "+
				"(it holds a Homebox key and can write to your inventory); "+
				"generate one with: openssl rand -base64 32", c.ListenAddr)
	}

	// With client credentials accepted, an authenticated caller decides which
	// host this server connects to. On loopback that is the operator talking to
	// themselves; bound wider it is a request forwarder, and the shared token
	// is the only thing in front of it. Refuse the combination that has neither.
	if c.AllowClientHomebox && c.APIToken == "" && !isLoopback(c.ListenAddr) {
		return c, fmt.Errorf(
			"ALLOW_CLIENT_HOMEBOX=true with LISTEN_ADDR=%s and no BOXWRIGHT_API_TOKEN would let "+
				"anything on the network make this server connect to any address it can reach",
			c.ListenAddr)
	}

	switch c.AIProvider {
	case "openai", "ollama":
		// These name no host and no model of their own -- the whole point is
		// that they front someone else's endpoint -- so both must be given.
		if c.AIBaseURL == "" {
			return c, fmt.Errorf("AI_BASE_URL is required when AI_PROVIDER=%s", c.AIProvider)
		}
		if c.AIModel == "" {
			return c, fmt.Errorf("AI_MODEL is required when AI_PROVIDER=%s", c.AIProvider)
		}
	case "anthropic":
		// The endpoint and model both default (see internal/ai/anthropic.go).
		// A credential is the one thing we cannot invent, and only when the
		// request actually goes to Anthropic: a gateway may authenticate itself.
		if c.AIBaseURL == "" && c.AIAPIKey == "" {
			return c, fmt.Errorf(
				"AI_API_KEY is required when AI_PROVIDER=anthropic " +
					"(or set AI_BASE_URL to a gateway that supplies the credential)")
		}
	case "claude-code":
		// Nothing is required, and nothing would help: the CLI supplies its own
		// endpoint and its own credential from an interactive login, so
		// AI_BASE_URL and AI_API_KEY have nowhere to go. AI_MODEL is optional
		// and defers to the CLI's own configured model when unset.
		//
		// Warned about at startup rather than merely documented, because the
		// costs of choosing it are invisible from the config file: it bills
		// ~16x a direct API call per photo, and it cannot work at all on a
		// machine where nobody has signed in to the CLI -- which includes every
		// container and every headless server this might otherwise be deployed
		// on. Not an error: on the owner's laptop it is exactly the right tool
		// for grading identification quality.
		slog.Default().Warn(
			"AI_PROVIDER=claude-code is a development-only evaluation path: it shells out to the " +
				"Claude Code CLI, costs roughly 16x a direct API call per photo (every call re-sends " +
				"the Claude Code harness), and requires an interactively authenticated `claude` on this " +
				"machine. Use AI_PROVIDER=anthropic for anything deployed.")
	case "none":
	default:
		return c, fmt.Errorf(
			"AI_PROVIDER must be openai, ollama, anthropic, claude-code, or none; got %q", c.AIProvider)
	}

	ttl := getenv("BOX_CACHE_TTL", "5m")
	d, err := time.ParseDuration(ttl)
	if err != nil {
		return c, fmt.Errorf("invalid BOX_CACHE_TTL %q: %w", ttl, err)
	}
	c.BoxCacheTTL = d
	return c, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// isLoopback reports whether binding to addr keeps the server unreachable from
// other machines. The empty string and "0.0.0.0" mean every interface.
func isLoopback(addr string) bool {
	switch addr {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// LogValue redacts secrets so a config dump cannot leak the Homebox key, the
// AI key or the shared token into logs.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("listenAddr", c.ListenAddr),
		slog.String("port", c.Port),
		slog.String("homeboxBaseURL", c.HomeboxBaseURL),
		slog.String("homeboxAPIKey", redact(c.HomeboxAPIKey)),
		slog.String("aiProvider", c.AIProvider),
		slog.String("aiBaseURL", c.AIBaseURL),
		slog.String("aiAPIKey", redact(c.AIAPIKey)),
		slog.String("aiModel", c.AIModel),
		slog.String("apiToken", redact(c.APIToken)),
		slog.Bool("allowClientHomebox", c.AllowClientHomebox),
		slog.Bool("homeboxWatch", c.HomeboxWatch),
		slog.Duration("boxCacheTTL", c.BoxCacheTTL),
	)
}

func redact(s string) string {
	if s == "" {
		return "(unset)"
	}
	return "(set)"
}
