package api

import (
	"context"
	"math"
	"time"

	"boxwright/internal/homebox"
	"boxwright/internal/placement"
)

// fillObservation is how full a container was seen to be, WITH the item going
// into it inside: the answer to "how full is it now?" asked at the container,
// or a LiDAR photo of it taken there. An observation is a fact, and replaces
// whatever Boxwright had estimated.
type fillObservation struct {
	Pct    float64 `json:"pct"`
	Source string  `json:"source"` // placement.FillObserved or placement.FillLidar
	At     string  `json:"at"`     // RFC3339, when it was observed
}

// valid reports whether the observation can be believed. One that cannot is
// ignored rather than failing the entry: the item still has to be filed.
func (o *fillObservation) valid() (time.Time, bool) {
	if o == nil || o.Pct < 0 || o.Pct > 150 || math.IsNaN(o.Pct) {
		return time.Time{}, false
	}
	if o.Source != placement.FillObserved && o.Source != placement.FillLidar {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, o.At)
	return at, err == nil
}

// boxFill is everything one catalog request says about one container's fill.
type boxFill struct {
	entries    []int            // indexes into the request, for fillError
	litres     float64          // what the entries that were CREATED take up
	observed   *fillObservation // the latest valid observation among them
	observedAt time.Time
}

// fillUpdate decides what one container's fill becomes, given its fields as
// they are now. Pure, so every rule is a table test:
//
//   - An observation is written as it stands -- source, time and all -- unless
//     the container already holds one at least as recent. That is what makes a
//     resend harmless, and stops a capture that sat in the offline queue
//     overnight from overwriting what someone saw this morning.
//   - Otherwise the litres filed are added to a KNOWN fill, as an estimate, if
//     the capture came after the last observation; an observation made later
//     already includes the item. fillCheckedAt stays where it was, so it keeps
//     saying how stale the fact underneath is.
//   - An unknown fill stays unknown: a sum with an unknown term is unknown.
//
// Whole percentages, because nobody has checked that a Homebox number field
// round-trips a fraction.
func fillUpdate(cur homebox.FieldSet, f boxFill, capturedAt time.Time) []homebox.CustomField {
	checked := time.Time{}
	if v, ok := cur.Text(fieldFillChecked); ok {
		checked, _ = time.Parse(time.RFC3339, v)
	}

	if f.observed != nil {
		if !checked.IsZero() && !f.observedAt.After(checked) {
			return nil
		}
		return fillFields(f.observed.Pct, f.observed.Source, f.observed.At)
	}

	capacity, ok := cur.Number(fieldCapacityL)
	if !ok || capacity <= 0 || f.litres <= 0 {
		return nil
	}
	source, _ := cur.Text(fieldFillSource)
	fill, hasFill := cur.Number(fieldFillPct)
	switch source {
	case placement.FillObserved, placement.FillLidar, placement.FillEstimated:
	default:
		hasFill = false
	}
	if !hasFill {
		return nil
	}
	if !checked.IsZero() && !capturedAt.IsZero() && capturedAt.Before(checked) {
		return nil
	}
	v, _ := cur.Text(fieldFillChecked)
	return fillFields(fill+f.litres/capacity*100, placement.FillEstimated, v)
}

func fillFields(pct float64, source, checkedAt string) []homebox.CustomField {
	return []homebox.CustomField{
		{Name: fieldFillPct, Type: homebox.FieldTypeNumber, NumberValue: math.Round(pct)},
		{Name: fieldFillSource, Type: homebox.FieldTypeText, TextValue: source},
		{Name: fieldFillChecked, Type: homebox.FieldTypeText, TextValue: checkedAt},
	}
}

// writeFills records, once per container, what this request did to its fill,
// and returns the containers it wrote so the cache can take their fields from
// what Homebox sent back.
//
// Best effort per container, like the item metadata: a fill that failed to
// save is reported on each entry that went into that container and nothing is
// retried. The failure leaves the fill an UNDER-count -- the items are in
// Homebox, the addition is not -- which is the safe direction: a container
// reads roomier than it is until someone looks, never fuller.
func (s *Server) writeFills(ctx context.Context, inst *instance, fills map[string]*boxFill, capturedAt time.Time, results []catalogResult) map[string]homebox.Entity {
	inst.fillMu.Lock()
	defer inst.fillMu.Unlock()

	written := map[string]homebox.Entity{}
	for boxID, f := range fills {
		if f.observed == nil && inst.fillUntracked(boxID) {
			// Nothing observed and nothing known to add to: reading the
			// container back would cost a request per filing to learn that.
			continue
		}
		e, err := inst.hb.UpdateFields(ctx, boxID, func(cur homebox.FieldSet) []homebox.CustomField {
			return fillUpdate(cur, *f, capturedAt)
		})
		if err != nil {
			s.log.Warn("fill level not recorded; the container will read roomier than it is until checked",
				"box", boxID, "err", err)
			for _, i := range f.entries {
				results[i].FillError = err.Error()
			}
			continue
		}
		written[boxID] = e
	}
	return written
}

// fillUntracked reports whether the cached index knows this container has no
// fill to add to. A container it does not know about -- one just created, or
// an index not yet loaded -- is not known to be untracked, and gets its write.
func (inst *instance) fillUntracked(boxID string) bool {
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	if !inst.loaded {
		return false
	}
	for _, b := range inst.boxes {
		if b.ID == boxID {
			return !b.TracksFill()
		}
	}
	return false
}

// patchBoxFields replaces a cached container's recorded fields with the ones
// Homebox just returned, so the next recommendation sees the fill it wrote
// without a rebuild. False when the box is not in the index, which the
// caller treats like any other patch that could not be applied.
func (inst *instance) patchBoxFields(boxID string, e homebox.Entity) bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if !inst.loaded {
		return false
	}
	for i := range inst.boxes {
		if inst.boxes[i].ID != boxID {
			continue
		}
		// Copy-on-write, as in patchBox: readers may hold the old slice.
		boxes := make([]placement.Box, len(inst.boxes))
		copy(boxes, inst.boxes)
		b := boxes[i]
		b.ContainerType, b.CapacityL, b.InteriorCm = "", nil, nil
		b.FillPct, b.FillSource, b.FillCheckedAt = nil, "", ""
		b.Access, b.HeavySafe, b.FragileSafe = "", nil, nil
		readContainerFields(&b, e.FieldSet())
		b.SetLegacyUnits()
		boxes[i] = b
		inst.boxes = boxes
		inst.generation++
		return true
	}
	return false
}
