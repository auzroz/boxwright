//go:build e2e

// Package e2e drives the real server binary against a real Homebox.
//
// It is the scripted end-to-end gate in docs/RELEASE.md: identify, then
// recommend, then catalog with a photo, a partial failure resent and
// deduplicated by boxwrightKey, /api/v1/status, and the change feed -- each
// checked through Boxwright's API AND read back from Homebox directly, so a
// response that claims success is not taken on its word.
//
// It needs a Homebox v0.26.x that allows registration and has
// HBOX_AUTH_API_KEY_PEPPER set, and nothing else: it registers its own user
// (so reruns against one Homebox never see each other's data), mints an API
// key, builds cmd/server, and stands up a stub OpenAI-compatible vision model
// so identification is deterministic and costs nothing.
//
//	docker compose up -d homebox          # or any Homebox v0.26.x
//	make e2e                              # E2E_HOMEBOX_URL defaults to :7745
//
// Behind a build tag so `go test ./...` never needs a Homebox.
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const apiToken = "e2e-api-token"

// stubItem is what the stub vision model says is in every photo.
var stubItem = map[string]any{
	"name": "E2E cordless drill", "category": "tools", "sizeBucket": "M",
	"fragile": false, "bulky": false, "weightClass": "medium",
	"notes": "", "confidence": 0.9, "quantity": 1,
}

func TestEndToEnd(t *testing.T) {
	hbURL := strings.TrimRight(envOr("E2E_HOMEBOX_URL", "http://127.0.0.1:7745"), "/")
	ctx := context.Background()

	// --- Homebox: a fresh user, an API key, and somewhere to put things.
	hb := newHomebox(t, hbURL)
	shelf := hb.createLocation(t, "E2E shelf", "")
	bin := hb.createLocation(t, "E2E bin", shelf)

	// --- The stub model, recording what it was sent.
	stub := newStubModel(t)

	// --- The real binary.
	bw := startBackend(t, map[string]string{
		"HOMEBOX_BASE_URL":    hbURL + "/api",
		"HOMEBOX_API_KEY":     hb.apiKey,
		"BOXWRIGHT_API_TOKEN": apiToken,
		"AI_PROVIDER":         "openai",
		"AI_BASE_URL":         stub.url,
		"AI_API_KEY":          "e2e-not-a-key",
		"AI_MODEL":            "e2e-stub",
		"HOMEBOX_WATCH":       "true",
	})

	t.Run("status", func(t *testing.T) {
		if code := bw.do(t, "GET", "/api/v1/status", "", nil, nil, false); code != http.StatusUnauthorized {
			t.Fatalf("status without the token: %d, want 401", code)
		}
		// The web UI syncs its theme to the user's settings. Set it the way
		// the browser does, with the session, and read it back through the
		// backend, which holds only the API key: that is the measurement of
		// whether an API key can read settings at all.
		if code := hb.call(t, "PUT", "/api/v1/users/self/settings", hb.session,
			map[string]any{"theme": "forest"}, nil); code/100 != 2 {
			t.Fatalf("set the theme: %d", code)
		}
		var st struct {
			Version        string `json:"version"`
			APIVersion     int    `json:"apiVersion"`
			Identification bool   `json:"identification"`
			AIProvider     string `json:"aiProvider"`
			ClientHomebox  bool   `json:"clientHomebox"`
			Homebox        struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			} `json:"homebox"`
			HomeboxTheme string `json:"homeboxTheme"`
		}
		bw.mustJSON(t, "GET", "/api/v1/status", nil, &st)
		if st.HomeboxTheme != "forest" {
			t.Errorf("homeboxTheme %q through the API key, want the forest the session set", st.HomeboxTheme)
		}
		if st.Version != "e2e" || st.APIVersion != 1 || !st.Identification || st.AIProvider != "openai" ||
			st.ClientHomebox || !st.Homebox.OK {
			t.Fatalf("status = %+v", st)
		}
	})

	t.Run("change feed connects to the real Homebox", func(t *testing.T) {
		deadline := time.Now().Add(20 * time.Second)
		var last string
		for time.Now().Before(deadline) {
			var h struct {
				ChangeFeed string `json:"changeFeed"`
			}
			if bw.do(t, "GET", "/healthz", "", nil, &h, false) == http.StatusOK {
				last = h.ChangeFeed
				if last == "connected" {
					return
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("changeFeed never reached connected; last %q", last)
	})

	t.Run("choose where things may go", func(t *testing.T) {
		body := map[string]any{"locations": []map[string]any{
			{"id": shelf, "eligible": true}, {"id": bin, "eligible": true},
		}}
		var res struct {
			Results []struct {
				ID    string `json:"id"`
				Error string `json:"error"`
			} `json:"results"`
		}
		bw.mustJSON(t, "PUT", "/api/v1/locations", body, &res)
		for _, r := range res.Results {
			if r.Error != "" {
				t.Fatalf("selecting %s: %s", r.ID, r.Error)
			}
		}
		// Read back from Homebox itself: the selection is a custom field there.
		for _, id := range []string{shelf, bin} {
			if got := hb.textField(t, id, "boxwrightPlacement"); got != "true" {
				t.Fatalf("%s boxwrightPlacement = %q in Homebox, want \"true\"", id, got)
			}
		}
	})

	photo := testJPEG(t)
	var drafts []map[string]any

	t.Run("identify", func(t *testing.T) {
		var res struct {
			Items []map[string]any `json:"items"`
		}
		code := bw.multipart(t, "/api/v1/identify", nil, photo, &res)
		if code != http.StatusOK {
			t.Fatalf("identify: %d", code)
		}
		if len(res.Items) != 1 || res.Items[0]["name"] != stubItem["name"] {
			t.Fatalf("identify items = %v", res.Items)
		}
		// The model was shown THIS photo, labelled as what it is.
		if got := stub.lastImage(); !bytes.Equal(got.data, photo) || got.mime != "image/jpeg" {
			t.Fatalf("model received %d bytes as %q; want the %d-byte photo as image/jpeg",
				len(got.data), got.mime, len(photo))
		}
		drafts = res.Items
	})

	t.Run("recommend", func(t *testing.T) {
		var res struct {
			Recommendations []struct {
				Candidates []struct {
					Box struct {
						ID string `json:"id"`
					} `json:"box"`
				} `json:"candidates"`
				NewContainer *struct {
					Label string `json:"label"`
				} `json:"newContainer"`
			} `json:"recommendations"`
		}
		bw.mustJSON(t, "POST", "/api/v1/recommend", map[string]any{"items": drafts}, &res)
		if len(res.Recommendations) != 1 {
			t.Fatalf("%d recommendations for 1 item", len(res.Recommendations))
		}
		rec := res.Recommendations[0]
		// Two empty chosen locations: either may be scored, or the engine may
		// prefer a new container -- but it must answer with SOMETHING.
		if len(rec.Candidates) == 0 && rec.NewContainer == nil {
			t.Fatal("no candidates and no new-container suggestion")
		}
		for _, c := range rec.Candidates {
			if c.Box.ID != shelf && c.Box.ID != bin {
				t.Fatalf("candidate %s is not a location the user chose", c.Box.ID)
			}
		}
	})

	// Two entries, one aimed at a box that does not exist: a partial failure.
	const captureID = "e2e-capture-1"
	sander := map[string]any{}
	for k, v := range stubItem {
		sander[k] = v
	}
	sander["name"] = "E2E orbital sander"
	// A box deleted in Homebox after the app cached it: a real-looking id
	// that no longer exists. (NOT the nil UUID -- Homebox reads that as "no
	// parent" and files at the top level; see the next subtest.)
	missing := "7f3c9a51-1111-4222-8333-944455556666"
	var drillID string

	t.Run("catalog: the nil UUID is refused, not filed at the top level", func(t *testing.T) {
		var res struct {
			Results []catalogResult `json:"results"`
		}
		code := bw.multipart(t, "/api/v1/catalog", map[string]any{"captureId": "e2e-nil", "entries": []map[string]any{
			{"item": drafts[0], "entryId": "e2e-nil-0", "boxId": "00000000-0000-0000-0000-000000000000"},
		}}, photo, &res)
		if code != http.StatusBadRequest {
			t.Fatalf("catalog into the nil UUID: %d, want 400 (%+v)", code, res.Results)
		}
		hb.assertTopLevelItems(t, 0)
	})

	t.Run("catalog: a partial failure lands what it can, with the photo", func(t *testing.T) {
		res := bw.catalog(t, captureID, []map[string]any{
			{"item": drafts[0], "entryId": captureID + "-0", "boxId": bin},
			{"item": sander, "entryId": captureID + "-1", "boxId": missing},
		}, photo)
		if len(res) != 2 {
			t.Fatalf("%d results for 2 entries", len(res))
		}
		if res[0].Error != "" || res[0].Entity.ID == "" || !res[0].FieldsWritten || !res[0].PhotoUploaded || res[0].Deduped {
			t.Fatalf("drill result = %+v", res[0])
		}
		if res[1].Error == "" {
			t.Fatalf("an entry aimed at a missing box landed: %+v", res[1])
		}
		drillID = res[0].Entity.ID

		// Homebox's own account of what landed.
		e := hb.entity(t, drillID)
		if e.Parent == nil || e.Parent.ID != bin {
			t.Fatalf("drill parent = %+v, want %s", e.Parent, bin)
		}
		if len(e.Attachments) == 0 {
			t.Fatal("drill has no attachment in Homebox")
		}
		if got := hb.textField(t, drillID, "boxwrightKey"); got != captureID+"-0" {
			t.Fatalf("drill boxwrightKey = %q, want %q", got, captureID+"-0")
		}
	})

	t.Run("catalog: the resend files the fixed entry and deduplicates the landed one", func(t *testing.T) {
		// What the app does after the user fixes the refused entry: the same
		// capture, the same entry ids. The drill must not be filed twice.
		res := bw.catalog(t, captureID, []map[string]any{
			{"item": drafts[0], "entryId": captureID + "-0", "boxId": bin},
			{"item": sander, "entryId": captureID + "-1", "boxId": bin},
		}, photo)
		if !res[0].Deduped || res[0].Entity.ID != drillID {
			t.Fatalf("drill on resend = %+v, want deduped to %s", res[0], drillID)
		}
		if res[1].Error != "" || res[1].Deduped || res[1].Entity.ID == "" {
			t.Fatalf("sander on resend = %+v", res[1])
		}
		hb.assertItemsIn(t, bin, map[string]int{"E2E cordless drill": 1, "E2E orbital sander": 1})
	})

	t.Run("catalog: a response lost in transit is not a second copy", func(t *testing.T) {
		// Exactly the same request again, as the offline queue sends it after
		// a timeout that hid a success.
		res := bw.catalog(t, captureID, []map[string]any{
			{"item": drafts[0], "entryId": captureID + "-0", "boxId": bin},
			{"item": sander, "entryId": captureID + "-1", "boxId": bin},
		}, photo)
		if !res[0].Deduped || !res[1].Deduped {
			t.Fatalf("identical resend was not deduplicated: %+v", res)
		}
		hb.assertItemsIn(t, bin, map[string]int{"E2E cordless drill": 1, "E2E orbital sander": 1})
	})

	t.Run("record the bin as a container of a known size and fill", func(t *testing.T) {
		var res struct {
			Results []struct {
				ID    string `json:"id"`
				Error string `json:"error"`
			} `json:"results"`
		}
		bw.mustJSON(t, "PUT", "/api/v1/containers", map[string]any{
			"ids": []string{bin},
			"set": map[string]any{
				"containerType": "E2E crate", "capacityL": 60,
				"interiorCm": map[string]any{"l": 50, "w": 30, "h": 40},
				"fill":       map[string]any{"pct": 20, "source": "observed", "at": time.Now().UTC().Format(time.RFC3339)},
			},
		}, &res)
		if len(res.Results) != 1 || res.Results[0].Error != "" {
			t.Fatalf("results = %+v", res.Results)
		}
		// Read back from Homebox itself: number fields round-trip as numbers.
		if got := hb.numberField(t, bin, "capacityL"); got != 60 {
			t.Fatalf("capacityL = %v in Homebox, want 60", got)
		}
		if got := hb.numberField(t, bin, "fillPct"); got != 20 {
			t.Fatalf("fillPct = %v in Homebox, want 20", got)
		}
		if got := hb.textField(t, bin, "interiorCm"); got != "50x40x30" {
			t.Fatalf("interiorCm = %q in Homebox, want 50x40x30", got)
		}
		// Written beside what was there, never over it.
		if got := hb.textField(t, bin, "boxwrightPlacement"); got != "true" {
			t.Fatalf("recording a size cleared the placement flag: %q", got)
		}
	})

	t.Run("catalog into a known container adds to its fill, once", func(t *testing.T) {
		// An M item is 8 L: 20% of 60 L plus 8 L is 33%.
		level := map[string]any{"name": "E2E spirit level", "category": "tools", "sizeBucket": "M",
			"weightClass": "light", "quantity": 1}
		entries := []map[string]any{{"item": level, "entryId": "e2e-fill-0", "boxId": bin}}
		if res := bw.catalog(t, "e2e-fill", entries, photo); res[0].Error != "" || res[0].FillError != "" {
			t.Fatalf("result = %+v", res[0])
		}
		if got := hb.numberField(t, bin, "fillPct"); got != 33 {
			t.Fatalf("fillPct = %v in Homebox, want 33", got)
		}
		if got := hb.textField(t, bin, "fillSource"); got != "estimated" {
			t.Fatalf("fillSource = %q, want estimated", got)
		}
		if res := bw.catalog(t, "e2e-fill", entries, photo); !res[0].Deduped {
			t.Fatalf("resend was not deduplicated: %+v", res[0])
		}
		if got := hb.numberField(t, bin, "fillPct"); got != 33 {
			t.Fatalf("a resend moved fillPct to %v, want 33", got)
		}
	})

	t.Run("recommend again: the box that now holds tools is a candidate", func(t *testing.T) {
		var res struct {
			Recommendations []struct {
				Candidates []struct {
					Box struct {
						ID string `json:"id"`
					} `json:"box"`
				} `json:"candidates"`
			} `json:"recommendations"`
		}
		bw.mustJSON(t, "POST", "/api/v1/recommend", map[string]any{"items": drafts}, &res)
		for _, c := range res.Recommendations[0].Candidates {
			if c.Box.ID == bin {
				return
			}
		}
		t.Fatalf("the bin holding two tools is not a candidate for a third: %+v", res.Recommendations[0].Candidates)
	})

	_ = ctx
}

// ---------------------------------------------------------------------------
// Homebox, driven directly
// ---------------------------------------------------------------------------

type homeboxHarness struct {
	url     string
	session string // "Bearer ..." from login, for the calls an API key cannot make
	apiKey  string // hb_..., what the backend is given
	locType string
}

func newHomebox(t *testing.T, base string) *homeboxHarness {
	t.Helper()
	h := &homeboxHarness{url: base}
	var st struct {
		Build struct {
			Version string `json:"version"`
		} `json:"build"`
		AllowRegistration bool `json:"allowRegistration"`
	}
	if code := h.call(t, "GET", "/api/v1/status", "", nil, &st); code != http.StatusOK {
		t.Fatalf("no Homebox at %s (status %d); start one, see the package comment", base, code)
	}
	// The release binary reports "0.26.2" and the container image "v0.26.2";
	// both are the same release.
	if !strings.HasPrefix(strings.TrimPrefix(st.Build.Version, "v"), "0.26.") {
		t.Fatalf("Homebox %q; this gate is for v0.26.x", st.Build.Version)
	}
	if !st.AllowRegistration {
		t.Fatal("Homebox does not allow registration; the harness needs a user of its own")
	}
	t.Logf("Homebox %s at %s", st.Build.Version, base)

	email := fmt.Sprintf("e2e-%d@example.com", time.Now().UnixNano())
	password := "e2e-password-not-secret"
	if code := h.call(t, "POST", "/api/v1/users/register", "",
		map[string]any{"name": "Boxwright e2e", "email": email, "password": password}, nil); code/100 != 2 {
		t.Fatalf("register: %d", code)
	}
	var login struct {
		Token string `json:"token"`
	}
	if code := h.call(t, "POST", "/api/v1/users/login", "",
		map[string]any{"username": email, "password": password, "stayLoggedIn": true}, &login); code != http.StatusOK {
		t.Fatalf("login: %d", code)
	}
	h.session = login.Token
	var key struct {
		Token string `json:"token"`
	}
	if code := h.call(t, "POST", "/api/v1/users/self/api-keys", h.session,
		map[string]any{"name": "boxwright-e2e"}, &key); code/100 != 2 || !strings.HasPrefix(key.Token, "hb_") {
		t.Fatalf("api key: %d %q (is HBOX_AUTH_API_KEY_PEPPER set?)", code, key.Token)
	}
	h.apiKey = key.Token

	var types []struct {
		ID         string `json:"id"`
		IsLocation bool   `json:"isLocation"`
	}
	h.call(t, "GET", "/api/v1/entity-types", h.session, nil, &types)
	for _, et := range types {
		if et.IsLocation {
			h.locType = et.ID
		}
	}
	if h.locType == "" {
		t.Fatal("no location entity type in a fresh group")
	}
	return h
}

func (h *homeboxHarness) createLocation(t *testing.T, name, parent string) string {
	t.Helper()
	var e struct {
		ID string `json:"id"`
	}
	body := map[string]any{"name": name, "entityTypeId": h.locType}
	if parent != "" {
		body["parentId"] = parent
	}
	if code := h.call(t, "POST", "/api/v1/entities", h.session, body, &e); code/100 != 2 || e.ID == "" {
		t.Fatalf("create location %q: %d", name, code)
	}
	return e.ID
}

type hbEntity struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Parent *struct {
		ID string `json:"id"`
	} `json:"parent"`
	Fields []struct {
		Name        string  `json:"name"`
		TextValue   string  `json:"textValue"`
		NumberValue float64 `json:"numberValue"`
	} `json:"fields"`
	Attachments []json.RawMessage `json:"attachments"`
}

func (h *homeboxHarness) entity(t *testing.T, id string) hbEntity {
	t.Helper()
	var e hbEntity
	if code := h.call(t, "GET", "/api/v1/entities/"+id, h.session, nil, &e); code != http.StatusOK {
		t.Fatalf("get entity %s: %d", id, code)
	}
	return e
}

// numberField reads a number custom field back from Homebox itself.
func (h *homeboxHarness) numberField(t *testing.T, id, name string) float64 {
	t.Helper()
	for _, f := range h.entity(t, id).Fields {
		if f.Name == name {
			return f.NumberValue
		}
	}
	return -1
}

func (h *homeboxHarness) textField(t *testing.T, id, name string) string {
	t.Helper()
	for _, f := range h.entity(t, id).Fields {
		if f.Name == name {
			return f.TextValue
		}
	}
	return ""
}

// assertItemsIn checks the exact item names under a location, counted.
func (h *homeboxHarness) assertItemsIn(t *testing.T, parent string, want map[string]int) {
	t.Helper()
	var page struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	q := url.Values{"parentIds": {parent}, "pageSize": {"100"}}
	if code := h.call(t, "GET", "/api/v1/entities?"+q.Encode(), h.session, nil, &page); code != http.StatusOK {
		t.Fatalf("list items in %s: %d", parent, code)
	}
	got := map[string]int{}
	for _, it := range page.Items {
		got[it.Name]++
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("items in Homebox under %s = %v, want %v", parent, got, want)
	}
}

// assertTopLevelItems counts items with no parent at all, which is where an
// entry lands if Homebox is handed the nil UUID as its parent.
func (h *homeboxHarness) assertTopLevelItems(t *testing.T, want int) {
	t.Helper()
	var page struct {
		Items []struct {
			Name   string          `json:"name"`
			Parent json.RawMessage `json:"parent"`
		} `json:"items"`
	}
	if code := h.call(t, "GET", "/api/v1/entities?pageSize=100", h.session, nil, &page); code != http.StatusOK {
		t.Fatalf("list items: %d", code)
	}
	got := 0
	for _, it := range page.Items {
		if len(it.Parent) == 0 || string(it.Parent) == "null" {
			got++
		}
	}
	if got != want {
		t.Fatalf("%d items at the top level in Homebox, want %d", got, want)
	}
}

func (h *homeboxHarness) call(t *testing.T, method, path, auth string, in, out any) int {
	t.Helper()
	return doJSON(t, method, h.url+path, auth, in, out)
}

// ---------------------------------------------------------------------------
// The backend, as a real process
// ---------------------------------------------------------------------------

type backend struct{ url string }

func startBackend(t *testing.T, env map[string]string) *backend {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "boxwright")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=e2e", "-o", bin, "boxwright/cmd/server")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build cmd/server: %v", err)
	}

	port := freePort(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "LISTEN_ADDR=127.0.0.1", "PORT="+port, "BOX_CACHE_TTL=5m")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	logFile, err := os.Create(filepath.Join(dir, "backend.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start backend: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
		}
		_ = logFile.Close()
		if t.Failed() {
			if b, err := os.ReadFile(logFile.Name()); err == nil {
				t.Logf("backend log:\n%s", b)
			}
		}
	})

	b := &backend{url: "http://127.0.0.1:" + port}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(b.url + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return b
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("backend did not answer /healthz within 20s")
	return nil
}

func (b *backend) do(t *testing.T, method, path, _ string, in, out any, authed bool) int {
	t.Helper()
	auth := ""
	if authed {
		auth = "Bearer " + apiToken
	}
	return doJSON(t, method, b.url+path, auth, in, out)
}

func (b *backend) mustJSON(t *testing.T, method, path string, in, out any) {
	t.Helper()
	if code := b.do(t, method, path, "", in, out, true); code/100 != 2 {
		t.Fatalf("%s %s: %d", method, path, code)
	}
}

// multipart sends an optional JSON payload part and the photo, as the app does.
func (b *backend) multipart(t *testing.T, path string, payload any, photo []byte, out any) int {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if payload != nil {
		raw, _ := json.Marshal(payload)
		_ = mw.WriteField("payload", string(raw))
	}
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="image"; filename="capture.jpg"`)
	hdr.Set("Content-Type", "image/jpeg")
	part, _ := mw.CreatePart(hdr)
	_, _ = part.Write(photo)
	_ = mw.Close()

	req, _ := http.NewRequest("POST", b.url+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("POST %s: %d, undecodable body %q: %v", path, resp.StatusCode, raw, err)
		}
	}
	return resp.StatusCode
}

type catalogResult struct {
	Deduped bool `json:"deduped"`
	Entity  struct {
		ID string `json:"id"`
	} `json:"entity"`
	FieldsWritten bool   `json:"fieldsWritten"`
	PhotoUploaded bool   `json:"photoUploaded"`
	PhotoError    string `json:"photoError"`
	FillError     string `json:"fillError"`
	Error         string `json:"error"`
}

func (b *backend) catalog(t *testing.T, captureID string, entries []map[string]any, photo []byte) []catalogResult {
	t.Helper()
	var res struct {
		Results []catalogResult `json:"results"`
	}
	code := b.multipart(t, "/api/v1/catalog", map[string]any{"captureId": captureID, "entries": entries}, photo, &res)
	// 201 when at least one entry landed; a resend that lands nothing new is
	// still an answer, so any 2xx is read and judged per entry.
	if code/100 != 2 {
		t.Fatalf("catalog: %d", code)
	}
	return res.Results
}

// ---------------------------------------------------------------------------
// The stub vision model
// ---------------------------------------------------------------------------

type seenImage struct {
	mime string
	data []byte
}

type stubModel struct {
	url  string
	mu   sync.Mutex
	last seenImage
}

// newStubModel serves POST /chat/completions the way an OpenAI-compatible
// endpoint does, pulling the image back out of the request so the test can
// check the backend forwarded exactly what the app uploaded.
func newStubModel(t *testing.T) *stubModel {
	t.Helper()
	s := &stubModel{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		// The image travels as a data: URL somewhere in the message content.
		if i := bytes.Index(raw, []byte("data:")); i >= 0 {
			rest := string(raw[i+len("data:"):])
			if semi, comma := strings.Index(rest, ";base64,"), strings.IndexByte(rest, ','); semi > 0 && comma > semi {
				end := strings.IndexByte(rest[comma+1:], '"')
				if end > 0 {
					data, err := base64.StdEncoding.DecodeString(rest[comma+1 : comma+1+end])
					if err == nil {
						s.mu.Lock()
						s.last = seenImage{mime: rest[:semi], data: data}
						s.mu.Unlock()
					}
				}
			}
		}
		content, _ := json.Marshal(map[string]any{"items": []any{stubItem}})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(content)}}},
		})
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

func (s *stubModel) lastImage() seenImage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

// testJPEG is a real, decodable photo -- Homebox thumbnails attachments, so a
// handful of random bytes would test the wrong failure.
func testJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 5), 120, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func doJSON(t *testing.T, method, target, auth string, in, out any) int {
	t.Helper()
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: undecodable body %q: %v", method, target, raw, err)
		}
	}
	return resp.StatusCode
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
