package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"boxwright/internal/homebox"
)

// The app's connection screen reads this to tell someone, before they walk
// into a storage unit, whether each link in the chain works.
func TestStatusReportsEachLinkInTheChain(t *testing.T) {
	for _, tc := range []struct {
		name         string
		provider     string
		version      string
		homeboxCode  int
		clientHB     bool
		wantIdentify bool
		wantHomebox  bool
		wantVersion  string
	}{
		{"all good", "anthropic", "0.1.0", http.StatusOK, false, true, true, "0.1.0"},
		{"manual entry only", "none", "0.1.0", http.StatusOK, false, false, true, "0.1.0"},
		{"homebox rejects the token", "ollama", "0.1.0", http.StatusUnauthorized, false, true, false, "0.1.0"},
		{"unstamped build", "none", "", http.StatusOK, true, false, true, "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/entity-types" {
					t.Errorf("status probed %s; it should only list entity types", r.URL.Path)
				}
				if tc.homeboxCode != http.StatusOK {
					http.Error(w, "nope", tc.homeboxCode)
					return
				}
				json.NewEncoder(w).Encode([]homebox.EntityType{{ID: "loc", Name: "Location", IsLocation: true}})
			}))
			t.Cleanup(upstream.Close)

			s := New(homebox.New(upstream.URL+"/api", "tok"), mustProvider(t), time.Minute, discardLogger())
			s.SetAbout(About{Version: tc.version, AIProvider: tc.provider})
			s.AllowClientCredentials(tc.clientHB)
			s.RequireToken("s3cret")

			req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
			req.Header.Set("Authorization", "Bearer s3cret")
			rr := httptest.NewRecorder()
			s.Routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status %d, want 200 even when Homebox is down: %s", rr.Code, rr.Body)
			}
			var got statusResponse
			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Version != tc.wantVersion {
				t.Errorf("version %q, want %q", got.Version, tc.wantVersion)
			}
			if got.APIVersion != APIVersion {
				t.Errorf("apiVersion %d, want %d", got.APIVersion, APIVersion)
			}
			if got.Identification != tc.wantIdentify {
				t.Errorf("identification %v, want %v", got.Identification, tc.wantIdentify)
			}
			if got.ClientHomebox != tc.clientHB {
				t.Errorf("clientHomebox %v, want %v", got.ClientHomebox, tc.clientHB)
			}
			if got.Homebox.OK != tc.wantHomebox {
				t.Errorf("homebox.ok %v, want %v (error %q)", got.Homebox.OK, tc.wantHomebox, got.Homebox.Error)
			}
			if !got.Homebox.OK && got.Homebox.Error == "" {
				t.Error("an unreachable Homebox must say why, or the screen has nothing to show")
			}
		})
	}
}

// Status sits under /api, so reaching it proves the token. That is half its
// job: /healthz cannot tell a wrong token from a right one.
func TestStatusRequiresTheAPIToken(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	s := newTestServer(t, f)
	s.RequireToken("s3cret")

	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status %d without a token, want 401", rr.Code)
	}
}

// A request naming its own Homebox on a backend that refuses client
// credentials gets the same refusal every other endpoint gives, so the
// connection screen surfaces it before any capture does.
func TestStatusRefusesClientHomeboxWhenNotAllowed(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	s := newTestServer(t, f)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set(headerHomeboxToken, "someone-elses")
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400: %s", rr.Code, rr.Body)
	}
}
