package api

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// APIVersion is the contract generation served under /api/v1. It changes only
// when an existing field changes meaning or disappears; adding a field does
// not bump it. The app compares it against the generation it was built for,
// so an old build talking to a newer backend can say so instead of failing
// in a way that looks like bad signal.
const APIVersion = 1

// statusProbeTimeout bounds the Homebox check. The app calls this from a
// settings screen while someone waits, and a Homebox that takes longer than
// this to list its entity types is worth reporting as a problem anyway.
const statusProbeTimeout = 10 * time.Second

// themeTTL is how long a Homebox's theme is kept. A change in the web UI
// reaches the app within this, which is soon enough for a colour.
const themeTTL = 10 * time.Minute

// About describes this deployment for GET /api/v1/status. Set once from main.
type About struct {
	// Version is the build's version, stamped at link time. "dev" otherwise.
	Version string
	// AIProvider is the configured AI_PROVIDER, so the app can say up front
	// that identification is off rather than discovering it on the first photo.
	AIProvider string
}

// SetAbout records what GET /api/v1/status reports about this build.
func (s *Server) SetAbout(a About) { s.about = a }

type statusResponse struct {
	Version    string `json:"version"`
	APIVersion int    `json:"apiVersion"`
	// Identification is false when every item has to be entered by hand.
	Identification bool   `json:"identification"`
	AIProvider     string `json:"aiProvider"`
	// ClientHomebox says whether this backend accepts a Homebox URL and token
	// from the app. When false, the app must not offer to send them.
	ClientHomebox bool          `json:"clientHomebox"`
	Homebox       homeboxStatus `json:"homebox"`
	// HomeboxTheme is the web UI theme of the Homebox user these credentials
	// belong to ("homebox", "forest", ...), so the app can match its colours.
	// Empty when it could not be read; the app then keeps its own.
	HomeboxTheme string `json:"homeboxTheme,omitempty"`
}

type homeboxStatus struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// handleStatus answers "will this work?" for the app's connection screen.
//
// /healthz already says the process is up and needs no credential. This is
// the authenticated half: reaching it at all proves the API token, and it
// makes one cheap authenticated call to the Homebox this request resolves to,
// so a wrong Homebox token shows up while someone is looking at the settings
// screen rather than as a failed catalog in a storage unit.
//
// A Homebox that cannot be reached is a 200 with homebox.ok false, not a 502:
// the question was answered, and the answer is what the screen shows.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := statusResponse{
		Version:        s.about.Version,
		APIVersion:     APIVersion,
		Identification: s.about.AIProvider != "" && s.about.AIProvider != "none",
		AIProvider:     s.about.AIProvider,
		ClientHomebox:  s.allowClientCredentials,
	}
	if resp.Version == "" {
		resp.Version = "dev"
	}

	inst, err := s.instanceFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if inst.hb == nil {
		resp.Homebox.Error = errNoHomebox.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), statusProbeTimeout)
	defer cancel()
	// Entity types: small, authenticated, and read by nothing that caches, so
	// the probe neither warms nor disturbs the box index.
	if _, err := inst.hb.ListEntityTypes(ctx); err != nil {
		resp.Homebox.Error = err.Error()
	} else {
		resp.Homebox.OK = true
		resp.HomeboxTheme = inst.homeboxTheme(ctx, time.Now())
	}
	writeJSON(w, http.StatusOK, resp)
}

// homeboxTheme returns the cached web-UI theme, reading it again once it is
// older than themeTTL. A failure is not an error anywhere: the theme is a
// colour, and /status answers whether things WORK.
func (inst *instance) homeboxTheme(ctx context.Context, now time.Time) string {
	inst.themeMu.Lock()
	defer inst.themeMu.Unlock()
	if !inst.themeAt.IsZero() && now.Sub(inst.themeAt) < themeTTL {
		return inst.theme
	}
	settings, err := inst.hb.SelfSettings(ctx)
	if err != nil {
		inst.theme = ""
	} else {
		inst.theme = settings.Theme
	}
	inst.themeAt = now
	return inst.theme
}

var errNoHomebox = errors.New("this server has no Homebox of its own; set a Homebox URL and token in the app")
