// Package api exposes the backend HTTP API consumed by the mobile app.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"boxwright/internal/ai"
	"boxwright/internal/homebox"
	"boxwright/internal/placement"
)

// maxImageBytes caps uploaded photos (10 MiB).
const (
	// Homebox's own default upload cap is 10 MB (HBOX_WEB_MAX_FILE_UPLOAD), so
	// accepting more than that only defers the failure to a worse place.
	maxImageBytes = 10 << 20
	// A little headroom over the image for the JSON part and MIME framing.
	maxUploadBytes = maxImageBytes + (1 << 20)
	// JSON-only requests carry no photo and need nothing like that much.
	maxJSONBytes = 1 << 20
	// How much of a multipart body is buffered before spilling to disk.
	multipartMemory = 8 << 20
)

// errImageTooLarge maps to 413 rather than a generic 400.
var errImageTooLarge = errors.New("image exceeds the 10 MB limit; capture at a lower resolution")

// readCapped reads at most max bytes and reports an error if there are more.
//
// io.ReadAll(io.LimitReader(...)) silently TRUNCATES instead, which hands a
// corrupt JPEG to the vision model and to Homebox with a 200 and no
// indication anything went wrong.
func readCapped(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errImageTooLarge
	}
	return data, nil
}

// Server wires handlers to Homebox and the AI provider.
//
// It holds no box index of its own. Everything cached about a Homebox lives on
// an instance keyed by the credentials that reached it (see instance.go), so a
// second person's containers can never be served from the first person's
// cache.
type Server struct {
	identifier ai.Identifier
	log        *slog.Logger

	cacheTTL time.Duration
	// apiToken, when non-empty, is required on every /api request. Empty means
	// loopback-only operation, which config.Load already enforces.
	apiToken string

	// defaults are the operator's own Homebox, from the environment. This is
	// the self-hosted product: with no per-request credentials, every request
	// resolves here.
	defaults credentials
	// allowClientCredentials permits a request to name its own Homebox. Off by
	// default, so a backend that was not told to accept client credentials
	// never fetches a URL a client chose.
	allowClientCredentials bool

	// about is what GET /api/v1/status reports; see status.go.
	about About

	instMu    sync.Mutex
	instances map[string]*instance

	// watch is the Homebox change-feed watcher, or nil when none is running.
	// Atomic because StartWatching is wiring that may land while Routes is
	// already serving, and /healthz reads it from a request goroutine.
	watch atomic.Pointer[watcher]
}

// New constructs a Server around one operator-configured Homebox.
//
// The client is taken rather than a URL and token so existing callers and
// tests keep working unchanged; its credentials are read back off it to key
// the default instance.
func New(hb *homebox.Client, identifier ai.Identifier, cacheTTL time.Duration, log *slog.Logger) *Server {
	s := &Server{
		identifier: identifier,
		cacheTTL:   cacheTTL,
		log:        log,
		instances:  map[string]*instance{},
	}
	// A nil client is legal and means "no Homebox of its own": the multi-user
	// deployment, where every request brings its own credentials, and the
	// tests that exercise cache behaviour with no upstream at all.
	if hb != nil {
		s.defaults = credentials{baseURL: hb.BaseURL(), token: hb.Token()}
	}
	// Seed the default instance with the client we were handed, rather than
	// building an equivalent one: a test may have configured it.
	inst := &instance{hb: hb, key: s.defaults.key(), used: time.Now()}
	s.instances[inst.key] = inst
	return s
}

// AllowClientCredentials lets requests carry their own Homebox URL and token.
//
// Off by default and deliberately a separate call, because turning it on
// changes what this service will connect to: an authenticated caller can name
// any address the server can reach. See instance.go.
func (s *Server) AllowClientCredentials(allow bool) { s.allowClientCredentials = allow }

// RequireToken sets the shared secret required on /api requests. Empty
// disables the check, which config.Load permits only for a loopback bind.
func (s *Server) RequireToken(token string) { s.apiToken = token }

// Routes returns the HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("POST /api/v1/identify", s.handleIdentify)
	mux.HandleFunc("POST /api/v1/recommend", s.handleRecommend)
	mux.HandleFunc("POST /api/v1/catalog", s.handleCatalog)
	mux.HandleFunc("GET /api/v1/boxes", s.handleBoxes)
	mux.HandleFunc("GET /api/v1/locations", s.handleLocations)
	mux.HandleFunc("PUT /api/v1/locations", s.handlePutLocations)
	mux.HandleFunc("POST /api/v1/locations/adopt", s.handleAdoptLocations)
	mux.HandleFunc("PUT /api/v1/containers", s.handlePutContainers)
	mux.HandleFunc("GET /api/v1/categories", s.handleCategories)
	mux.HandleFunc("GET /api/v1/entity-types", s.handleEntityTypes)
	return logMiddleware(s.log, s.authMiddleware(mux))
}

// authMiddleware gates /api behind the shared secret when one is configured.
// /healthz stays open so a reachability probe needs no credential.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken == "" || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		// Constant time, so the token cannot be recovered a byte at a time.
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.apiToken)) != 1 {
			writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid API token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// changeFeed is the one thing here that is not a constant, and it is three
	// words on purpose: connected, degraded or disabled, all of which an
	// operator can act on. No counters, no timestamps, and above all nothing
	// naming a credential or a URL -- this handler is exempt from the API
	// token gate, so everything it prints is public.
	writeJSON(w, http.StatusOK, map[string]string{
		"status":     "ok",
		"changeFeed": s.changeFeed(),
	})
}

// errNothingIdentified is the empty-array case: the model answered, and its
// answer was "nothing here is packable". It joins ErrNoProvider on the 422
// manual-entry path because the app has exactly one thing to do about either.
var errNothingIdentified = errors.New("no items were identified in this photo; enter the item details manually")

// handleIdentify accepts multipart form field "image" and returns one
// ItemDraft per distinct object in the photo. When AI_PROVIDER=none, or when
// nothing packable was recognised, it returns 422 with a manual-entry hint.
func (s *Server) handleIdentify(w http.ResponseWriter, r *http.Request) {
	inst, err := s.instanceFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	defer func() {
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll()
		}
	}()
	if err = r.ParseMultipartForm(multipartMemory); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("expected multipart form with an image field: %w", err))
		return
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("missing image field"))
		return
	}
	defer file.Close()

	data, err := readCapped(file, maxImageBytes)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errImageTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeErr(w, status, err)
		return
	}
	mime := header.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/jpeg"
	}

	// The model cannot pick from a vocabulary it has not been shown, and the
	// vocabulary that matters is the user's tags, not our seed list. This never
	// fails: it degrades to the seeds rather than lose the capture.
	items, err := s.identifier.Identify(r.Context(), data, mime, s.identifyVocabulary(r.Context(), inst))
	if errors.Is(err, ai.ErrNoProvider) {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if len(items) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, errNothingIdentified)
		return
	}
	// Where the items are is only worth passing on if the model was looking
	// rather than guessing. Logged rather than silent: "no crops appeared" is
	// otherwise indistinguishable from a model that simply did not answer, and
	// the difference is the one thing an operator can act on -- by using a
	// model that can point at things.
	if dropped := placement.DropFabricatedRegions(items); dropped > 0 {
		s.log.Info("identify: discarded invented item regions; the review screen will show the "+
			"whole photo for every item. This model cannot locate objects -- a larger one can.",
			"regions", dropped, "items", len(items))
	}
	// items is non-empty here, so it can never marshal to null -- which would
	// red-screen a client calling .map() on it.
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// recommendRequest is every item from one capture, scored together.
type recommendRequest struct {
	Items []placement.ItemDraft `json:"items"`
}

// handleRecommend scores cached boxes for every posted ItemDraft, as one
// batch. recommendations[i] is items[i].
func (s *Server) handleRecommend(w http.ResponseWriter, r *http.Request) {
	inst, err := s.instanceFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	var req recommendRequest
	if err = json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid recommend request: %w", err))
		return
	}
	if len(req.Items) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("items is required: one draft per item to place"))
		return
	}
	boxes, stale, err := inst.boxIndex(r.Context(), s.cacheTTL)
	if err != nil && len(boxes) == 0 {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("box index unavailable: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"recommendations": recommendBatch(req.Items, boxes, placement.DefaultWeights()),
		"stale":           stale, // true when serving cache past TTL (offline tolerance)
	})
}

// recommendBatch is the reason /recommend takes an array at all: the items
// from one photo have to see each other.
//
// Scored one request at a time, five items from a shelf shot all get told to
// use the same half-empty box, because none of them knows the other four are
// about to be put in it. So each item's placement is RESERVED against the
// working index before the next is scored: capacity the first consumed is gone
// for the second, and the category the first added counts towards the second's
// like-with-like affinity, which is how a box of tools comes to exist out of
// one photo instead of one item.
//
// Deterministic by construction: items are processed in the order the client
// sent them, boxes are scored in index order with a name tie-break inside
// placement.Recommend, and nothing here iterates a map to decide anything.
func recommendBatch(items []placement.ItemDraft, boxes []placement.Box, w placement.Weights) []placement.Recommendation {
	// A COPY. boxIndex hands out the cached slice itself and this is a read
	// handler: reserving against the shared index would corrupt it for every
	// other request, and would double-count against the generation-counted
	// patch that /catalog applies for real.
	working := make([]placement.Box, len(boxes))
	copy(working, boxes)

	out := make([]placement.Recommendation, 0, len(items))
	for _, item := range items {
		// Normalize here as well as inside Recommend, which works on its own
		// copy: the reservation below needs the same size bucket, quantity
		// and category the scoring used.
		item.Normalize()
		rec := placement.Recommend(item, working, w)
		out = append(out, rec)

		// Reserve against the top candidate only, and only when it is one the
		// engine actually stands behind. With a NewContainer suggestion the
		// headline answer is "none of these fits", so consuming capacity from
		// a box we just advised against would penalise the next item for a
		// placement nobody is going to make.
		if rec.NewContainer == nil && len(rec.Candidates) > 0 {
			reserve(working, rec.Candidates[0].Box.ID, item)
		}
	}
	return out
}

// reserve applies an item to a box in the working index, as if the user had
// accepted the recommendation.
//
// Copy-on-write, not mutation in place, for the same reason patchBox is: the
// Box values already handed out inside earlier Candidates share this
// Categories map, and those recommendations are marshalled after the whole
// batch is scored. Mutating the map would rewrite the counts the first item
// was shown to have been scored against.
func reserve(boxes []placement.Box, boxID string, item placement.ItemDraft) {
	for i := range boxes {
		if boxes[i].ID != boxID {
			continue
		}
		b := boxes[i]
		cats := make(map[string]int, len(b.Categories)+1)
		for k, v := range b.Categories {
			cats[k] = v
		}
		// MatchCategory, because that is the key the index counts under and
		// the key the engine looks up.
		cats[placement.MatchCategory(item.Category)] += item.Quantity
		b.Categories = cats
		// The space it takes, where the fill is known; and in any case no
		// longer empty -- the second item from the same photo must not also
		// be offered this box as a fresh start.
		b.AddLitres(item.NeedLitres())
		b.ItemCount += item.Quantity
		b.SetLegacyUnits()
		boxes[i] = b
		return
	}
}

// handleEntityTypes proxies Homebox's entity types. The app needs these to
// create a new container: omitting entityTypeId on create auto-resolves a
// NON-location type, which would silently make the "new box" an item.
func (s *Server) handleEntityTypes(w http.ResponseWriter, r *http.Request) {
	inst, err := s.instanceFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	types, err := inst.hb.ListEntityTypes(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entityTypes": types})
}

func (s *Server) handleBoxes(w http.ResponseWriter, r *http.Request) {
	inst, err := s.instanceFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	boxes, stale, err := inst.boxIndex(r.Context(), s.cacheTTL)
	if err != nil && len(boxes) == 0 {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"boxes": boxes,
		"stale": stale,
		// The user's own container types, derived from the containers that
		// carry one, so the app can offer them when setting up a container
		// or naming a new one -- offline too, since the app caches this.
		"containerTypes": placement.ContainerTypes(boxes),
		// What an item of each size bucket is taken to occupy, so the app's
		// offline picker uses the engine's figures instead of a copy.
		"sizeLitres": placement.NominalLitres,
	})
}

// boxIndex returns the cached box list, refreshing past TTL, and records that
// somebody asked. Every request path goes through here.
func (inst *instance) boxIndex(ctx context.Context, cacheTTL time.Duration) ([]placement.Box, bool, error) {
	// Stamped before anything can return early, so a cache hit counts as a
	// read: this is what the change-feed watcher's pre-warm gate reads to tell
	// a Homebox somebody is working against from one nobody has touched.
	//
	// It lives in this wrapper rather than in the body below because the
	// watcher's own pre-warm goes through that body, and a pre-warm that
	// stamped this would re-arm the very gate it is gated on: one real request
	// opened it, and a trickle of foreign edits then kept an idle backend
	// rebuilding a ~21s index for a client that had gone hours ago.
	inst.lastRead.Store(time.Now().UnixNano())
	return inst.boxIndexUnstamped(ctx, cacheTTL)
}

// boxIndexUnstamped is boxIndex for the one caller that is not a reader: the
// change-feed pre-warm. Same commit protocol -- there is exactly one of those,
// deliberately -- minus the read stamp above.
func (inst *instance) boxIndexUnstamped(ctx context.Context, cacheTTL time.Duration) ([]placement.Box, bool, error) {
	inst.mu.RLock()
	// Keyed on whether a fetch has ever succeeded, not on the slice being
	// non-nil: an inventory with zero boxes is a legitimate result, and the
	// old nil check made it defeat the cache and refetch on every request.
	loaded := inst.loaded
	fresh := loaded && !inst.fetched.IsZero() && time.Since(inst.fetched) < cacheTTL
	cached := inst.boxes
	startedAt := inst.generation
	inst.mu.RUnlock()
	if fresh {
		return cached, false, nil
	}

	boxes, err := inst.fetchBoxes(ctx)
	if err != nil {
		// Re-read the cache instead of deciding on the snapshot taken before
		// the fetch. A rebuild is ~21 seconds against a live instance, and
		// plenty happens in that window: another request can fill a cache that
		// was cold when this one started, and patchBox can correct a warm one.
		// Trusting the pre-fetch copy answered 502 with zero boxes to a request
		// that began cold while a complete index sat right here -- on
		// /recommend, which is the call somebody in a storage unit is actually
		// making -- and served a pre-patch slice to a warm one.
		inst.mu.RLock()
		loaded, cached = inst.loaded, inst.boxes
		inst.mu.RUnlock()
		if loaded {
			return cached, true, err
		}
		return []placement.Box{}, false, err
	}
	inst.mu.Lock()
	switch {
	case inst.generation == startedAt:
		inst.boxes = boxes
		inst.fetched = time.Now()
		inst.loaded = true
	case !inst.loaded:
		// Superseded, but nothing has ever loaded. Dropping it here left the
		// cache cold, and two things then failed together: patchBox refuses an
		// index that has never loaded, so every catalog write fell back to the
		// full rebuild it exists to avoid, and there was no stale data to fall
		// back on above, so the next upstream failure answered 502 instead of
		// serving the boxes we were holding right here. A POST /catalog
		// landing while the first rebuild after startup is still in flight was
		// enough to reach it. So publish the snapshot but leave fetched ZERO:
		// one we already know is superseded is still better than none at all,
		// as long as it is never marked fresh. Zero leaves the index due for
		// immediate refresh, so the write that superseded it is not ignored.
		inst.boxes = boxes
		inst.loaded = true
	}
	// A warm cache that was superseded keeps what it has instead: something was
	// written while this rebuild was in flight, so this result is already out
	// of date and committing it would resurrect the state that write corrected.
	// The next request rebuilds. Either way the caller still gets what we read,
	// which is the most recent view we actually have.
	inst.mu.Unlock()
	return boxes, false, nil
}

// invalidate marks the index due for refresh without discarding it: the
// existing boxes stay available as a stale fallback if that refresh fails.
func (inst *instance) invalidate() {
	inst.mu.Lock()
	inst.fetched = time.Time{}
	inst.generation++
	inst.mu.Unlock()
}

// boxDelta is the effect one catalogued item has on its box's cached counters.
// Category is already normalized and litres already multiplied out, so
// patchBox applies it without re-deriving anything the write path knows.
type boxDelta struct {
	boxID    string
	category string
	quantity int
	litres   float64
}

// patchBox applies one catalogued item to the cached box index in place of
// throwing the index away, and reports whether it could.
//
// A rebuild is 1 list call plus 2 per chosen location -- 161 sequential round
// trips and roughly 18 seconds for 80 containers against the live instance,
// which is latency, not work. handleCatalog used to invalidate(), so
// the very next /recommend paid that -- every item in a cataloguing session
// cost 4.3s for information we already had. We know exactly what changed: one
// item of a known category, size bucket and quantity went into one known box.
//
// false means the caller must fall back to invalidate():
//
//   - Nothing has ever loaded, so there is no index to patch.
//   - The target box is not in the index. A brand-new container is the normal
//     case, and it needs the full rebuild anyway: a location entering or
//     leaving the chosen set is not something a per-item delta can express.
func (inst *instance) patchBox(d boxDelta) bool {
	if d.boxID == "" || d.quantity < 1 {
		return false
	}

	inst.mu.Lock()
	defer inst.mu.Unlock()
	if !inst.loaded {
		return false
	}
	i := -1
	for n := range inst.boxes {
		if inst.boxes[n].ID == d.boxID {
			i = n
			break
		}
	}
	if i < 0 {
		return false
	}

	// Copy-on-write, not mutation in place. boxIndex hands the caller the
	// cached slice itself and releases the lock before the handler ranges over
	// it, so a reader can be scoring these Boxes -- and reading the Categories
	// map inside one -- while this runs. Writing through that shared memory is
	// a data race, and on the map a fatal one. Copying the backing array and
	// the single Box being touched (with a fresh Categories map) is bounded
	// work per catalog write and leaves every reader holding a consistent
	// snapshot. The untouched Boxes stay shared between the old and new
	// slices, which is sound precisely because this is the only writer and it
	// never mutates one in place.
	boxes := make([]placement.Box, len(inst.boxes))
	copy(boxes, inst.boxes)

	b := boxes[i]
	cats := make(map[string]int, len(b.Categories)+1)
	for k, v := range b.Categories {
		cats[k] = v
	}
	cats[d.category] += d.quantity
	b.Categories = cats
	b.ItemCount += d.quantity
	boxes[i] = b
	inst.boxes = boxes

	// Bump the generation for the same reason invalidate() does: a rebuild
	// that started before this patch is carrying a pre-patch snapshot, and
	// committing it would resurrect the state we just corrected -- the lost
	// invalidation bug wearing a different hat. It sees the generation move
	// and drops its result instead.
	//
	// fetched is deliberately NOT cleared. Keeping the cache serveable is the
	// whole point, and letting the TTL keep running from the last real fetch
	// means the periodic full refresh still lands on schedule, so any drift
	// self-heals: our size accounting against the read path's estimate, or
	// someone moving an item in the Homebox UI.
	inst.generation++
	return true
}

// Custom-field names Boxwright reads and writes on entities. The location
// ones describe a container; the item ones describe what was put in it.
// fieldPlacement, which decides whether a location is a candidate at all,
// lives in locations.go with the rest of that decision.
// See docs/ARCHITECTURE.md for the model.
const (
	fieldCapacityUnits = "capacityUnits" // legacy: neither read nor written; see fetchBoxes
	fieldContainerType = "containerType"
	fieldCapacityL     = "capacityL"
	fieldInteriorCm    = "interiorCm"
	fieldFillPct       = "fillPct"
	fieldFillSource    = "fillSource"
	fieldFillChecked   = "fillCheckedAt"
	fieldDimensionsCm  = "dimensionsCm"
	fieldDimsSource    = "dimensionsSource"
	fieldAccess        = "access"
	fieldHeavySafe     = "heavySafe"
	fieldFragileSafe   = "fragileSafe"
	fieldGridX         = "gridX"
	fieldGridY         = "gridY"
	fieldSizeBucket    = "sizeBucket"
	fieldWeightClass   = "weightClass"
	fieldFragile       = "fragile"
)

// boxRefreshTimeout bounds a whole index rebuild. The per-request timeout on
// the Homebox client is not enough on its own: a rebuild is 1 list call plus
// two per chosen location, so without an overall deadline a slow instance can
// hold a request for timeout x N.
const boxRefreshTimeout = 45 * time.Second

// fetchBoxes builds placement.Box records from the locations the user has
// opted in to automated placement.
//
// The set is the user's, not ours: a location is a candidate because its
// boxwrightPlacement field says so (see locations.go), and for no other
// reason. There is no rule here about depth, emptiness, or having children,
// because every such rule this code used to carry was generalised from one
// person's inventory and produced no candidates at all on inventories shaped
// differently.
//
// The opted-in set is fetched with the fields= filter, so the cost is
// proportional to what the user chose rather than to the size of their
// Homebox. That query is an optimisation and not the check: each location's
// own fields are read below and re-tested, so a server that ignores the filter
// makes this slow rather than wrong.
//
// Custom fields cost one detail call per candidate -- list rows never carry
// them (verified against v0.26.2) -- and we need them anyway to score the box.
func (inst *instance) fetchBoxes(ctx context.Context) ([]placement.Box, error) {
	ctx, cancel := context.WithTimeout(ctx, boxRefreshTimeout)
	defer cancel()

	isLoc := true
	locations, err := inst.hb.ListEntitiesByField(ctx, fieldPlacement, []string{placementYes}, &isLoc)
	if err != nil {
		return nil, err
	}

	out := []placement.Box{}
	for _, loc := range locations {
		detail, err := inst.hb.GetEntity(ctx, loc.ID)
		if err != nil {
			return nil, fmt.Errorf("location %q metadata: %w", loc.Name, err)
		}
		f := detail.FieldSet()
		if !eligibleForPlacement(f) {
			continue
		}

		b := placement.Box{
			ID:       loc.ID,
			Name:     loc.Name,
			ParentID: loc.ParentID(),
			// From the row's own parent reference rather than a lookup: the
			// filtered list contains only chosen locations, so a parent that
			// was not chosen would not be in it.
			Area: loc.ParentName(),
		}
		// capacityUnits is deliberately NOT read. The values in the wild were
		// Boxwright's own guesses -- a created container got its size
		// bucket's units, 2 or 4 or 8 -- and reading them back as a recorded
		// capacity turned a guess into grounds for exclusion. capacityL is
		// the recorded capacity; without it a box's capacity is unknown, and
		// unknown never excludes.
		readContainerFields(&b, f)

		// Contents: direct non-location children, for category affinity. A
		// chosen location may also hold other locations -- "Garage" with
		// shelves under it is a legitimate choice -- and those are skipped
		// here rather than counted against its capacity.
		children, err := inst.hb.ListEntities(ctx, []string{loc.ID}, nil)
		if err != nil {
			// Deliberately fail the whole refresh rather than skipping this
			// box or recording it as empty. A box with unknown contents looks
			// like maximum headroom to the engine and can win a ranking it
			// should have lost; failing lets boxIndex serve the last good
			// index with stale=true, which is both truthful and more useful.
			return nil, fmt.Errorf("box %q contents: %w", loc.Name, err)
		}
		cats := map[string]int{}
		count := 0
		for _, ch := range children {
			if ch.IsLocation() {
				continue
			}
			// Categories come from tags, which list rows carry in full.
			// An item may hold several tags (this instance has organisational
			// ones like "Important" alongside subject tags), so every tag
			// counts towards affinity and the engine matches on whichever the
			// item being placed belongs to.
			qty := ch.Quantity
			if qty < 1 {
				qty = 1
			}
			if len(ch.Tags) == 0 {
				cats["other"] += qty
			}
			for _, t := range ch.Tags {
				cats[placement.MatchCategory(t.Name)] += qty
			}
			// How many, and not how much: size is a custom field and absent
			// from list rows (reading it would cost a request per item -- 132
			// for one box here). This used to be charged as one M item per
			// unit, 2 units each against an assumed 8, which read any box
			// holding four things as full and excluded it.
			count += qty
		}
		b.Categories = cats
		b.ItemCount = count
		b.SetLegacyUnits()
		out = append(out, b)
	}
	return append(out, areasAbove(ctx, inst.hb, out)...), nil
}

// areasAbove returns the locations that CONTAIN the ones the user chose -- the
// garage above the totes, the storage unit above the shelves.
//
// They are not placement candidates in the ordinary sense and are never
// offered for something that goes in a container. They exist for one question:
// where does a lawn mower go. The answer a user gave when asked is "abstract
// it to a higher order location, ie a lawn mower is in the garage or storage
// unit 1" -- and that structure is already in their Homebox, so it is read
// rather than asked for again.
//
// This is NOT the shape-inference that Phase 1a removed. That inferred whether
// a location can hold items at all, which is a judgement and belongs to the
// user. This reads which location contains which, which is a fact.
//
// One extra list call, and only the ancestors of chosen locations: a room
// nobody keeps anything in is not somewhere to put a mower either.
func areasAbove(ctx context.Context, hb *homebox.Client, chosen []placement.Box) []placement.Box {
	if len(chosen) == 0 {
		return nil
	}
	isLoc := true
	all, err := hb.ListEntities(ctx, nil, &isLoc)
	if err != nil {
		// Losing the areas costs a bulky item its recommendation; losing the
		// whole index costs every item one. Degrade rather than fail.
		return nil
	}
	byID := make(map[string]homebox.Entity, len(all))
	for _, e := range all {
		byID[e.ID] = e
	}
	chosenIDs := make(map[string]bool, len(chosen))
	for _, b := range chosen {
		chosenIDs[b.ID] = true
	}

	// depthOf counts steps from the top of the hierarchy, so the garage sorts
	// ahead of the row inside it.
	depthOf := func(id string) int {
		depth := 0
		visited := map[string]bool{}
		for cur := id; cur != "" && !visited[cur]; {
			visited[cur] = true
			e, ok := byID[cur]
			if !ok {
				break
			}
			if e.ParentID() == "" {
				break
			}
			depth++
			cur = e.ParentID()
		}
		return depth
	}

	seen := map[string]bool{}
	areas := []placement.Box{}
	for _, b := range chosen {
		// Walk up from each chosen location. visited guards a parent cycle,
		// which Homebox should never produce and which would hang a rebuild.
		visited := map[string]bool{}
		for id := b.ParentID; id != "" && !visited[id]; {
			visited[id] = true
			parent, ok := byID[id]
			if !ok {
				break
			}
			// A chosen location is already in the index as a container. It
			// cannot also be the area above itself.
			if !seen[id] && !chosenIDs[id] {
				seen[id] = true
				areas = append(areas, placement.Box{
					ID:        parent.ID,
					Name:      parent.Name,
					ParentID:  parent.ParentID(),
					Area:      parent.ParentName(),
					IsArea:    true,
					AreaDepth: depthOf(parent.ID),
				})
			}
			id = parent.ParentID()
		}
	}
	return areas
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func logMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Info("request", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start).String())
	})
}

// readContainerFields reads what a user recorded about a container: its type,
// capacity, inside size, how full it is, how easy it is to reach and what it
// is safe for. Each is independent and each may be absent, which means
// unknown -- never zero, never a default. The index and the cache patches
// after a write both read through here, so they cannot disagree.
func readContainerFields(b *placement.Box, f homebox.FieldSet) {
	if v, ok := f.Text(fieldAccess); ok {
		b.Access = v
	}
	if v, ok := f.Bool(fieldHeavySafe); ok {
		b.HeavySafe = &v
	}
	if v, ok := f.Bool(fieldFragileSafe); ok {
		b.FragileSafe = &v
	}
	if v, ok := f.Number(fieldGridX); ok {
		b.GridX = int(v)
	}
	if v, ok := f.Number(fieldGridY); ok {
		b.GridY = int(v)
	}
	if v, ok := f.Text(fieldContainerType); ok {
		b.ContainerType = strings.TrimSpace(v)
	}
	if v, ok := f.Number(fieldCapacityL); ok && v > 0 {
		c := int(math.Round(v))
		b.CapacityL = &c
	}
	if v, ok := f.Text(fieldInteriorCm); ok {
		b.InteriorCm = placement.ParseDims(v)
	}
	if v, ok := f.Text(fieldFillSource); ok {
		b.FillSource = v
	}
	if v, ok := f.Number(fieldFillPct); ok && v >= 0 {
		b.FillPct = &v
	}
	if v, ok := f.Text(fieldFillChecked); ok {
		b.FillCheckedAt = v
	}
}
