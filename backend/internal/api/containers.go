package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"boxwright/internal/homebox"
	"boxwright/internal/placement"
)

// containerSet is what the app records about one or more containers at once --
// "these twelve are the same kind of container" -- and every field is
// optional: only what is present is written, and nothing else is touched.
type containerSet struct {
	ContainerType *string          `json:"containerType,omitempty"` // "" clears it
	CapacityL     *int             `json:"capacityL,omitempty"`
	InteriorCm    *placement.Dims  `json:"interiorCm,omitempty"`
	Access        *string          `json:"access,omitempty"`
	HeavySafe     *bool            `json:"heavySafe,omitempty"`
	FragileSafe   *bool            `json:"fragileSafe,omitempty"`
	Fill          *fillObservation `json:"fill,omitempty"`
}

type putContainersRequest struct {
	IDs []string     `json:"ids"`
	Set containerSet `json:"set"`
}

// containerResult is what became of one id, positionally: results[i] is ids[i].
type containerResult struct {
	ID    string          `json:"id"`
	Box   *placement.Box  `json:"box,omitempty"` // as the index now holds it
	Error string          `json:"error,omitempty"`
	e     *homebox.Entity // for the cache patch; never on the wire
}

// fields turns the request into custom fields, or explains what is wrong with
// it. Validated once, before anything is written: a bad value is the client's
// mistake for every id alike, not something to discover halfway through.
func (c containerSet) fields() ([]homebox.CustomField, error) {
	var out []homebox.CustomField
	if c.ContainerType != nil {
		out = append(out, homebox.CustomField{Name: fieldContainerType, Type: homebox.FieldTypeText, TextValue: strings.TrimSpace(*c.ContainerType)})
	}
	if c.CapacityL != nil {
		if *c.CapacityL <= 0 || *c.CapacityL > 10000 {
			return nil, fmt.Errorf("capacityL %d is not a container size in litres", *c.CapacityL)
		}
		out = append(out, homebox.CustomField{Name: fieldCapacityL, Type: homebox.FieldTypeNumber, NumberValue: float64(*c.CapacityL)})
	}
	if c.InteriorCm != nil {
		d, ok := c.InteriorCm.Normalize()
		if !ok {
			return nil, errors.New("interiorCm needs three sides between 0.5 and 300 cm")
		}
		out = append(out, homebox.CustomField{Name: fieldInteriorCm, Type: homebox.FieldTypeText, TextValue: d.String()})
	}
	if c.Access != nil {
		switch *c.Access {
		case "easy", "normal", "deep", "":
		default:
			return nil, fmt.Errorf("access %q is not easy, normal or deep", *c.Access)
		}
		out = append(out, homebox.CustomField{Name: fieldAccess, Type: homebox.FieldTypeText, TextValue: *c.Access})
	}
	if c.HeavySafe != nil {
		out = append(out, homebox.CustomField{Name: fieldHeavySafe, Type: homebox.FieldTypeBoolean, BooleanValue: *c.HeavySafe})
	}
	if c.FragileSafe != nil {
		out = append(out, homebox.CustomField{Name: fieldFragileSafe, Type: homebox.FieldTypeBoolean, BooleanValue: *c.FragileSafe})
	}
	if c.Fill != nil {
		if _, ok := c.Fill.valid(); !ok {
			return nil, errors.New("fill needs pct 0-150, source observed or lidar, and an RFC3339 time")
		}
	}
	if len(out) == 0 && c.Fill == nil {
		return nil, errors.New("set is empty: nothing to record")
	}
	return out, nil
}

// handlePutContainers records what the user knows about their containers.
//
// Positional and per-container, like /catalog: one container that cannot be
// written must not undo or hide the eleven that were. Each is one read and one
// write (UpdateFields), serialised with the fill writes of /catalog so the two
// cannot interleave on one container, and muted like any write of ours.
func (s *Server) handlePutContainers(w http.ResponseWriter, r *http.Request) {
	inst, err := s.instanceFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req putContainersRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode: %w", err))
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 500 {
		writeErr(w, http.StatusBadRequest, errors.New("ids: 1 to 500 container ids"))
		return
	}
	set, err := req.Set.fields()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	done := inst.beginWrite()
	defer done()
	inst.fillMu.Lock()
	results := make([]containerResult, len(req.IDs))
	for i, id := range req.IDs {
		results[i].ID = id
		if id == "" || isNilUUID(id) {
			results[i].Error = "not a container id"
			continue
		}
		e, err := inst.hb.UpdateFields(r.Context(), id, func(cur homebox.FieldSet) []homebox.CustomField {
			out := append([]homebox.CustomField{}, set...)
			if req.Set.Fill != nil {
				at, _ := req.Set.Fill.valid()
				out = append(out, fillUpdate(cur, boxFill{observed: req.Set.Fill, observedAt: at}, at)...)
			}
			return out
		})
		if err != nil {
			results[i].Error = err.Error()
			continue
		}
		results[i].e = &e
	}
	inst.fillMu.Unlock()

	written := 0
	rebuild := false
	for i := range results {
		if results[i].e == nil {
			continue
		}
		written++
		if !inst.patchBoxFields(results[i].ID, *results[i].e) {
			rebuild = true
		}
	}
	if rebuild {
		inst.invalidate()
	}
	if boxes, _, _ := inst.boxIndex(r.Context(), s.cacheTTL); len(boxes) > 0 {
		for i := range results {
			if b, ok := boxByID(boxes, results[i].ID); ok && results[i].Error == "" {
				results[i].Box = &b
			}
		}
	}

	status := http.StatusOK
	if written == 0 {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{"results": results})
}

func boxByID(boxes []placement.Box, id string) (placement.Box, bool) {
	for _, b := range boxes {
		if b.ID == id {
			return b, true
		}
	}
	return placement.Box{}, false
}
