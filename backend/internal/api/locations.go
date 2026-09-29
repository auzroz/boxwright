package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"boxwright/internal/homebox"
)

// Which locations Boxwright may file into is the USER'S choice, recorded in
// Homebox, and nothing else.
//
// It used to be inferred from the shape of the inventory -- a location was a
// container if it had no location children, was not an unannotated top-level
// entry, and so on. Every one of those rules was generalised from a single
// instance, and against a flat Homebox of twenty empty top-level boxes they
// produced zero candidates and an endless "buy a new container" suggestion.
// Users organise differently; some file into labelled boxes, some into "Attic" or
// "Shelf in Pantry", and someone with an elaborate scheme of their own must be
// able to point Boxwright at part of it and have the rest left alone.
//
// Eligibility lives in Homebox rather than in our config because Homebox is
// the system of record (principle 1): it is visible and editable in Homebox's
// own UI, it survives reinstalling this backend, and it stays attached to the
// user's instance rather than to ours -- which is what the per-user future
// needs, where the app carries its own Homebox credentials.
const (
	// fieldPlacement marks a location as a placement target.
	//
	// TEXT, not boolean, and deliberately so. Homebox's fields=Name=Value
	// filter works on text fields only (measured: a location holding
	// capacityUnits=8 is not returned by fields=capacityUnits=8). Storing this
	// as text is what makes the box-index rebuild cost proportional to the
	// locations the user OPTED IN, instead of one detail request per location
	// in the whole instance.
	fieldPlacement = "boxwrightPlacement"
	// placementYes is the only value that means eligible. Anything else,
	// including absent and including an empty string, means no.
	placementYes = "true"
	placementNo  = "false"
)

// placementFields are the location fields that mean "this was set up as a
// Boxwright container". Item-level fields (sizeBucket, weightClass, fragile)
// are deliberately not here.
var placementFields = []string{
	fieldCapacityUnits, fieldAccess, fieldHeavySafe, fieldFragileSafe, fieldGridX, fieldGridY,
}

// eligibleForPlacement reports whether a location's own fields opt it in.
// This is the authoritative test. The fields= query is only a way to avoid
// reading every location; what it returns is still checked here, so a server
// that ignores the filter costs time rather than correctness.
func eligibleForPlacement(fs homebox.FieldSet) bool {
	v, ok := fs.Text(fieldPlacement)
	return ok && v == placementYes
}

// annotatedForPlacement reports whether a location carries container metadata
// from before eligibility was explicit -- or from cmd/bootstrap, or from
// someone filling the fields in by hand in Homebox.
func annotatedForPlacement(fs homebox.FieldSet) bool {
	for _, name := range placementFields {
		if _, ok := fs[name]; ok {
			return true
		}
	}
	return false
}

// placementField renders the eligibility flag for a write.
func placementField(eligible bool) homebox.CustomField {
	v := placementNo
	if eligible {
		v = placementYes
	}
	return homebox.CustomField{Name: fieldPlacement, Type: homebox.FieldTypeText, TextValue: v}
}

// locationNode is one row of the picker: every location in the instance, with
// whether the user has opted it in.
//
// HasChildren and ItemCount are context for the person choosing, NOT filters.
// A location that holds other locations can still be a perfectly good place to
// put things, and this API no longer pretends to know otherwise.
type locationNode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ParentID    string `json:"parentId,omitempty"`
	ParentName  string `json:"parentName,omitempty"`
	Path        string `json:"path"`
	Depth       int    `json:"depth"`
	ItemCount   int    `json:"itemCount"`
	HasChildren bool   `json:"hasChildren"`
	Eligible    bool   `json:"eligible"`
}

// locationsTimeout bounds the write and migration paths, which are one or two
// Homebox calls per location the user touched. Generous because this is an
// explicit setup action a person is waiting on once, not a hot path.
const locationsTimeout = 4 * time.Minute

// handleLocations lists every location with its eligibility, for the picker.
//
// Two Homebox calls regardless of size: the whole location list, and the
// filtered list of opted-in ids. No per-location detail request, which is what
// keeps a picker usable on an instance with hundreds of locations.
func (s *Server) handleLocations(w http.ResponseWriter, r *http.Request) {
	inst, err := s.instanceFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), boxRefreshTimeout)
	defer cancel()

	isLoc := true
	all, err := inst.hb.ListEntities(ctx, nil, &isLoc)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("read locations: %w", err))
		return
	}
	chosen, err := inst.hb.ListEntitiesByField(ctx, fieldPlacement, []string{placementYes}, &isLoc)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("read placement selection: %w", err))
		return
	}
	eligible := make(map[string]bool, len(chosen))
	for _, e := range chosen {
		eligible[e.ID] = true
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"locations":     locationTree(all, eligible),
		"eligibleCount": len(eligible),
	})
}

// locationTree orders the locations for display: parents before their
// children, siblings by name, ties broken by id so the order never depends on
// map iteration.
func locationTree(all []homebox.Entity, eligible map[string]bool) []locationNode {
	nameByID := make(map[string]string, len(all))
	parentByID := make(map[string]string, len(all))
	hasChildren := make(map[string]bool, len(all))
	for _, e := range all {
		nameByID[e.ID] = e.Name
		parentByID[e.ID] = e.ParentID()
		if pid := e.ParentID(); pid != "" {
			hasChildren[pid] = true
		}
	}

	// pathOf walks up to the root. The visited set stops a parent cycle --
	// which Homebox should never produce, but which would otherwise hang the
	// only endpoint that could show the user what is wrong.
	pathOf := func(id string) (string, int) {
		parts := []string{}
		visited := map[string]bool{}
		for cur := id; cur != "" && !visited[cur]; cur = parentByID[cur] {
			visited[cur] = true
			name, ok := nameByID[cur]
			if !ok {
				break
			}
			parts = append([]string{name}, parts...)
		}
		if len(parts) == 0 {
			return "", 0
		}
		return strings.Join(parts, " > "), len(parts) - 1
	}

	out := make([]locationNode, 0, len(all))
	for _, e := range all {
		path, depth := pathOf(e.ID)
		out = append(out, locationNode{
			ID:          e.ID,
			Name:        e.Name,
			ParentID:    e.ParentID(),
			ParentName:  e.ParentName(),
			Path:        path,
			Depth:       depth,
			ItemCount:   e.ItemCount,
			HasChildren: hasChildren[e.ID],
			Eligible:    eligible[e.ID],
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// locationChoice is one row of a PUT: an id and what the user wants it to be.
type locationChoice struct {
	ID       string `json:"id"`
	Eligible bool   `json:"eligible"`
}

type locationsRequest struct {
	Locations []locationChoice `json:"locations"`
}

// locationResult reports one write. Partial success is normal -- a selection
// of eighty locations is eighty writes -- so the caller is told per id rather
// than by a status code.
type locationResult struct {
	ID       string `json:"id"`
	Eligible bool   `json:"eligible"`
	Error    string `json:"error,omitempty"`
}

// handlePutLocations records which locations Boxwright may file into.
//
// Writing "false" rather than removing the field is deliberate: SetFields
// merges and has no delete, and an explicit no is worth keeping anyway -- it
// stops the migration below from silently re-adopting a location the user
// turned off.
func (s *Server) handlePutLocations(w http.ResponseWriter, r *http.Request) {
	inst, err := s.instanceFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	var req locationsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid locations request: %w", err))
		return
	}
	if len(req.Locations) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("locations is required: one entry per location to change"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), locationsTimeout)
	defer cancel()

	// Homebox echoes every SetFields below back over the change feed. An
	// eighty-location selection would otherwise arrive as eighty mutation
	// frames, each one able to invalidate -- and so to kill -- the rebuild the
	// single invalidate() below correctly triggers.
	done := inst.beginWrite()
	defer done()

	results := make([]locationResult, 0, len(req.Locations))
	changed := false
	for _, c := range req.Locations {
		res := locationResult{ID: c.ID, Eligible: c.Eligible}
		if c.ID == "" {
			res.Error = "missing location id"
			results = append(results, res)
			continue
		}
		if _, err := inst.hb.SetFields(ctx, c.ID, []homebox.CustomField{placementField(c.Eligible)}); err != nil {
			res.Error = err.Error()
			s.log.Warn("placement selection write failed", "location", c.ID, "err", err)
		} else {
			changed = true
		}
		results = append(results, res)
	}
	// Any success changes which boxes exist, and a delta cannot express a box
	// appearing or disappearing. Rebuild rather than patch.
	if changed {
		inst.invalidate()
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// adoptedLocation is one location the migration turned on.
type adoptedLocation struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

// handleAdoptLocations marks every location already carrying container
// metadata as a placement target.
//
// A one-time, EXPLICIT migration, not a standing rule. Boxwright used to infer
// that an annotated location was a container; someone upgrading has eighty of
// them and should not have to tick eighty boxes, and someone who filled the
// fields in by hand meant the same thing. But inferring it on every read is
// exactly the behaviour this replaced, so it happens when asked and never
// otherwise.
//
// Idempotent, and it never overrides a decision: a location whose
// boxwrightPlacement is already set -- to true OR to false -- is left alone.
// This is the one path that reads every location's fields, one request each,
// because the metadata it looks for is numeric and boolean and therefore
// cannot be queried.
func (s *Server) handleAdoptLocations(w http.ResponseWriter, r *http.Request) {
	inst, err := s.instanceFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), locationsTimeout)
	defer cancel()

	isLoc := true
	all, err := inst.hb.ListEntities(ctx, nil, &isLoc)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("read locations: %w", err))
		return
	}

	// As in handlePutLocations: the writes in the loop echo back, and adopting
	// eighty locations at once is exactly the case this migration exists for.
	done := inst.beginWrite()
	defer done()

	adopted := []adoptedLocation{}
	scanned, marked := 0, 0
	changed := false
	for _, loc := range all {
		detail, err := inst.hb.GetEntity(ctx, loc.ID)
		if err != nil {
			adopted = append(adopted, adoptedLocation{ID: loc.ID, Name: loc.Name, Error: err.Error()})
			continue
		}
		scanned++
		fs := detail.FieldSet()
		if _, decided := fs[fieldPlacement]; decided {
			continue
		}
		if !annotatedForPlacement(fs) {
			continue
		}
		entry := adoptedLocation{ID: loc.ID, Name: loc.Name}
		if _, err := inst.hb.SetFields(ctx, loc.ID, []homebox.CustomField{placementField(true)}); err != nil {
			entry.Error = err.Error()
		} else {
			changed = true
			marked++
		}
		adopted = append(adopted, entry)
	}
	if changed {
		inst.invalidate()
	}
	// adopted carries the failures too, so the count is the successes only --
	// otherwise a run where every write failed reports as a successful
	// migration and the user never selects anything.
	writeJSON(w, http.StatusOK, map[string]any{
		"scanned": scanned,
		"marked":  marked,
		"adopted": adopted,
	})
}
