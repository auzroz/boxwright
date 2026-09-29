package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"boxwright/internal/homebox"
	"boxwright/internal/placement"
)

// catalogRequest files the items from ONE capture into Homebox.
//
// One photo, N entities: the review screen can produce eight entries from a
// shelf shot as easily as one from a drill, and each is placed independently,
// so two items out of the same photo may legitimately land in different boxes.
type catalogRequest struct {
	Entries []catalogEntry `json:"entries"`
	// CaptureID is the client's id for the capture these entries came from.
	// It is NOT the dedupe key and must never become one: after a partial
	// failure the app re-sends the same captureId with a DIFFERENT SUBSET of
	// entries, so anything keyed on the capture -- or on a position within it
	// -- matches the wrong entry and silently drops an item. That is worse
	// than the duplicate it would be trying to prevent. Dedupe is per entry,
	// on catalogEntry.EntryID. This rides along so the offline queue can
	// correlate a response with the queued capture that produced it.
	CaptureID string `json:"captureId,omitempty"`
	// CapturedAt is when the photo was taken (RFC3339). A fill observed AFTER
	// it already includes these items, so nothing is added to it. Absent from
	// older clients, and then taken as now.
	CapturedAt string `json:"capturedAt,omitempty"`
}

// catalogEntry is one reviewed item and the destination the user chose for it.
type catalogEntry struct {
	Item placement.ItemDraft `json:"item"`
	// EntryID is the client's idempotency key for THIS item, minted once when
	// the entry was built and persisted with it. It is stored on the created
	// entity as the boxwrightKey field, so a resend after a lost response
	// finds the item rather than filing it twice.
	//
	// Optional, and its absence is never an error: an older client, or one
	// that lost the key, is a reason to accept a possible duplicate, not a
	// reason to refuse a capture someone is standing in a storage unit
	// waiting on.
	EntryID      string               `json:"entryId,omitempty"`
	BoxID        string               `json:"boxId,omitempty"`
	EntityTypeID string               `json:"entityTypeId,omitempty"` // optional override
	NewContainer *newContainerRequest `json:"newContainer,omitempty"`
	// FillAfter is how full the destination was seen to be with this item in
	// it, when the user said (or measured). Optional: most entries have none,
	// and the fill is then estimated by adding the item's litres.
	FillAfter *fillObservation `json:"fillAfter,omitempty"`
}

// newContainerRequest accepts the engine's suggestion back from the client,
// including the Access it proposed and, when it named one of the user's own
// container types, that type -- so the container is created already knowing
// its size, and the next recommendation can score it.
type newContainerRequest struct {
	Label        string `json:"label"`
	ParentID     string `json:"parentId"`     // location to create the box under
	EntityTypeID string `json:"entityTypeId"` // a location-type id; resolved if empty
	SizeBucket   string `json:"sizeBucket"`
	Access       string `json:"access"`
	// Deliberately not part of containerKey: the same label under the same
	// parent is one container whatever size a later request says it is.
	ContainerType string          `json:"containerType,omitempty"`
	CapacityL     int             `json:"capacityL,omitempty"`
	InteriorCm    *placement.Dims `json:"interiorCm,omitempty"`
}

// catalogResult is what became of one entry. Results are positional:
// results[i] is entries[i], whether or not it landed.
//
// Per-entry rather than one verdict for the request, because a failure must
// neither roll back nor hide the entries that did land. Losing seven filed
// items to report the eighth's tag error would be the same silent data loss
// this whole change exists to end.
type catalogResult struct {
	// Entity is nil when nothing was created, so a client can tell "filed,
	// but the photo failed" from "not filed at all" without parsing strings.
	Entity        *homebox.Entity `json:"entity"`
	FieldsWritten bool            `json:"fieldsWritten"`
	FieldsError   string          `json:"fieldsError,omitempty"`
	PhotoUploaded bool            `json:"photoUploaded"`
	PhotoError    string          `json:"photoError,omitempty"`
	// FillError says the item landed but its container's fill level was not
	// updated. Nothing is retried: the container reads roomier than it is
	// until someone checks it, which is the safe way to be wrong.
	FillError string `json:"fillError,omitempty"`
	Error     string `json:"error,omitempty"`
	// Deduped says this entry was ALREADY filed by an earlier request carrying
	// the same entryId, and nothing was created now. Stated rather than
	// hidden: the flags above then describe the first attempt, which we did
	// not observe, and a client that wants to distinguish "filed" from "filed
	// just now" has no other way to.
	Deduped bool `json:"deduped,omitempty"`

	// clientFault separates "this entry was malformed" from "Homebox failed",
	// which is what picks the status code when NOTHING landed. Unexported, so
	// it never reaches the wire.
	clientFault bool
}

// badEntry is a result for an entry the client got wrong. Nothing was sent
// upstream, so there is nothing to report but the reason.
func badEntry(reason string) catalogResult {
	return catalogResult{Error: reason, clientFault: true}
}

// catalogBatch is the state shared by every entry in one request: the single
// photo, and the two things entries must not duplicate each other's work on.
type catalogBatch struct {
	photo     []byte
	photoName string
	// tags memoizes category -> tag for this request. Eight entries sharing a
	// category otherwise cost eight tag listings, and a category new to the
	// inventory would be looked up again after we just created it.
	tags map[string]homebox.Tag
	// containers memoizes new-container requests, so two items from one photo
	// that both need a new tools box get ONE box rather than two containers with
	// the same name in the same row.
	containers map[string]string
	// filed maps a boxwrightKey to the entity that already carries it, read in
	// ONE query per request before anything is created. An entry found here is
	// a resend of something that already landed.
	filed map[string]homebox.Entity
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	inst, err := s.instanceFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req, image, imageName, err := readCatalogRequest(r)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll() // ParseMultipartForm leaks temp files otherwise
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errImageTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeErr(w, status, err)
		return
	}
	if len(req.Entries) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("entries is required: one entry per item to file"))
		return
	}

	batch := &catalogBatch{
		photo:      image,
		photoName:  imageName,
		tags:       map[string]homebox.Tag{},
		containers: map[string]string{},
		filed:      s.alreadyFiled(r.Context(), inst, req.Entries),
	}

	// Everything from here to the patch below is our own writing, and Homebox
	// echoes each of those back over the change feed as a bare "something
	// changed". Muting it is what keeps a ten-item session from invalidating
	// its own index ten times and paying the rebuild patchBox exists to avoid.
	//
	// Begun after the batch, so the one dedupe query does not widen the window
	// for nothing, and released by defer -- which fires AFTER the patch block,
	// so no echo can slip in between the last upstream write and the patch
	// that already accounts for it.
	done := inst.beginWrite()
	defer done()

	results := make([]catalogResult, 0, len(req.Entries))
	deltas := make([]boxDelta, 0, len(req.Entries))
	fills := map[string]*boxFill{}
	landed, malformed := 0, 0
	for i, e := range req.Entries {
		res, delta := s.fileEntry(r.Context(), inst, e, batch)
		results = append(results, res)
		switch {
		case res.Entity != nil:
			landed++
			// A deduped entry created nothing, so it changed nothing. Its
			// delta was applied when it was really filed; appending a zero one
			// here would fail patchBox and force a needless full rebuild --
			// and appending a real one would charge the box twice.
			if !res.Deduped {
				deltas = append(deltas, delta)
			}
			noteFill(fills, i, e, res, delta)
		case res.clientFault:
			malformed++
		}
	}

	// Fill, once per container and after every entry, so eight things into
	// one container are one write. Inside the write mute, like the rest.
	capturedAt, err := time.Parse(time.RFC3339, req.CapturedAt)
	if err != nil {
		capturedAt = time.Now()
	}
	written := s.writeFills(r.Context(), inst, fills, capturedAt, results)

	// EVERY entity that landed has to reach the cached index, not just the
	// first: a patch that covered one of eight would leave the box looking
	// seven items emptier than it is, and the next recommendation would keep
	// steering items into a container that is already full.
	rebuild := false
	for _, d := range deltas {
		if !inst.patchBox(d) {
			rebuild = true
		}
	}
	for boxID, e := range written {
		if !inst.patchBoxFields(boxID, e) {
			rebuild = true
		}
	}
	if rebuild {
		inst.invalidate()
	}

	// 201 as soon as ANYTHING landed -- the durable outcome the user asked for
	// happened, and the per-entry results carry the rest. With nothing landed
	// the cause decides: the client's entries or Homebox.
	status := http.StatusBadGateway
	switch {
	case landed > 0:
		status = http.StatusCreated
	case malformed == len(results):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"results": results})
}

// alreadyFiled looks up everything in this request that may already exist,
// in two queries: one for the items, one for the containers.
//
// Batched deliberately. Doing it per entry would be N queries plus N detail
// reads for a photo of eight things, on the path where somebody is waiting.
// Items and containers need separate queries because isLocation is a different
// question, and omitting it returns items only rather than everything.
func (s *Server) alreadyFiled(ctx context.Context, inst *instance, entries []catalogEntry) map[string]homebox.Entity {
	itemKeys := make([]string, 0, len(entries))
	containerKeys := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.EntryID != "" {
			itemKeys = append(itemKeys, e.EntryID)
		}
		if e.NewContainer != nil {
			containerKeys = append(containerKeys, containerKeyID(*e.NewContainer))
		}
	}
	filed := s.filedByKey(ctx, inst, itemKeys, false)
	for k, v := range s.filedByKey(ctx, inst, containerKeys, true) {
		filed[k] = v
	}
	return filed
}

// dedupedResult reports an entry that a previous request already filed.
//
// The metadata and photo flags describe an attempt we did not observe, so they
// say "landed" rather than inventing a failure: the entity is in the system of
// record, which is what the user asked for, and Deduped says plainly that this
// request created nothing.
func dedupedResult(prior homebox.Entity, hasPhoto bool) catalogResult {
	e := prior
	return catalogResult{Entity: &e, FieldsWritten: true, PhotoUploaded: hasPhoto, Deduped: true}
}

// fileEntry files one item and returns its result plus the effect it had on
// its box, for the cache patch. A zero boxDelta means nothing landed.
//
// It never returns an error: an entry that fails is a result like any other,
// because the entries after it still have to be filed.
func (s *Server) fileEntry(ctx context.Context, inst *instance, e catalogEntry, batch *catalogBatch) (catalogResult, boxDelta) {
	e.Item.Normalize()
	if e.Item.Name == "" {
		return badEntry("item.name is required"), boxDelta{}
	}

	// A hit here returns NO boxDelta on purpose. The delta was applied when
	// this entry was really filed; applying it again would charge the box
	// twice for one item and steer the next recommendation away from a
	// container that has room.
	if e.EntryID != "" {
		if prior, ok := batch.filed[e.EntryID]; ok {
			return dedupedResult(prior, len(batch.photo) > 0), boxDelta{}
		}
		release, waited := inst.claimKey(ctx, e.EntryID)
		defer release()
		// Whether or not we waited: the up-front read may predate a create
		// that finished before we reached the claim. See rememberFiled.
		if prior, ok := inst.recentlyFiled(e.EntryID); ok {
			return dedupedResult(prior, len(batch.photo) > 0), boxDelta{}
		}
		if waited {
			// Somebody else was creating this while we queued. Look again
			// before creating a second one.
			for k, v := range s.filedByKey(ctx, inst, []string{e.EntryID}, false) {
				batch.filed[k] = v
			}
			if prior, ok := batch.filed[e.EntryID]; ok {
				return dedupedResult(prior, len(batch.photo) > 0), boxDelta{}
			}
		}
	}

	parentID := e.BoxID
	if e.NewContainer != nil {
		id, err := s.batchContainer(ctx, inst, *e.NewContainer, batch)
		if err != nil {
			return catalogResult{Error: err.Error()}, boxDelta{}
		}
		parentID = id
	}
	if parentID == "" {
		return badEntry("boxId or newContainer is required"), boxDelta{}
	}
	if isNilUUID(parentID) {
		// Measured against v0.26.2 (docs/HOMEBOX.md): a parentId that does not
		// exist is a 404, but the nil UUID is read as "no parent" and the item
		// is created at the top level with a 201. Sent on, this entry would
		// report success and land somewhere the user never chose.
		return badEntry(`boxId is the nil UUID, which Homebox reads as "no parent"`), boxDelta{}
	}

	tagIDs, category := s.categoryTag(ctx, inst, e.Item.Category, batch)

	created, err := inst.hb.CreateEntity(ctx, homebox.CreateEntityRequest{
		Name:         e.Item.Name,
		Description:  e.Item.Notes,
		Quantity:     e.Item.Quantity,
		ParentID:     parentID,
		EntityTypeID: e.EntityTypeID,
		TagIDs:       tagIDs,
	})
	if err != nil {
		return catalogResult{Error: err.Error()}, boxDelta{}
	}
	// Remembered before the claim is released, so the next holder sees it.
	inst.rememberFiled(e.EntryID, created)

	// Everything past this point is best effort. The item has landed in the
	// system of record, which is the durable outcome the user asked for;
	// failing the entry because a custom field or a photo did not stick would
	// throw that away to report a lesser problem.
	res := catalogResult{Entity: &created, FieldsWritten: true}

	// The key rides in the SetFields call that already happens, so idempotency
	// costs no extra write. There is a window between the create above and
	// this landing where the entity exists without its key; a crash inside it
	// means the next resend files a duplicate. Custom fields cannot be set on
	// create (POST has no fields), so the window cannot be closed -- and a
	// duplicate is the side to fail on.
	fields := itemFields(e.Item)
	if e.EntryID != "" {
		fields = append(fields, keyField(e.EntryID))
	}
	if _, err := inst.hb.SetFields(ctx, created.ID, fields); err != nil {
		s.log.Warn("catalog: writing item metadata failed", "item", created.Name, "err", err)
		res.FieldsWritten = false
		res.FieldsError = err.Error()
	}

	// The same bytes go up once per entity. Homebox attachments belong to one
	// entity -- there is no way to point several entities at one blob -- and
	// attaching the shelf photo to only the first item would leave the other
	// seven with no picture, which is most of what makes an inventory
	// browsable later. N is the number of items in one photo, single digits,
	// against a self-hosted instance, so the duplicated upload is the cheaper
	// half of the trade.
	if len(batch.photo) > 0 {
		if err := inst.hb.UploadAttachment(ctx, created.ID, batch.photoName, batch.photo, true); err != nil {
			s.log.Warn("catalog: photo upload failed", "item", created.Name, "err", err)
			res.PhotoError = err.Error()
		} else {
			res.PhotoUploaded = true
		}
	}

	// The space it takes is added to its container's fill once per box after
	// the whole batch (writeFills), and the cache takes the result from what
	// Homebox returns -- never from adding it here as well.
	return res, boxDelta{
		boxID:    parentID,
		category: category,
		quantity: e.Item.Quantity,
		litres:   e.Item.NeedLitres(),
	}
}

// categoryTag resolves the item's category to a Homebox tag, and reports the
// key the box index will count the item under.
//
// Category rides as a TAG, not a custom field. Custom fields are absent from
// list rows, so counting categories through them would cost one request per
// item -- 132 for a single box on this instance. Tags come back fully expanded
// on every row, and they are first-class in Homebox's own filtering, so the
// user gets something out of them too.
func (s *Server) categoryTag(ctx context.Context, inst *instance, category string, batch *catalogBatch) ([]string, string) {
	// fetchBoxes reads categories off the item's TAGS and files an untagged
	// item under "other", so when there is no tag the cache patch has to say
	// "other" too -- otherwise the patched index disagrees with what the next
	// full rebuild produces, and boxes drift in opposite directions for a
	// whole TTL.
	if category == "" {
		return nil, placement.MatchCategory("")
	}
	tag, ok := batch.tags[category]
	if !ok {
		var err error
		// placement.NormalizeCategory is the same fold GET /api/v1/categories
		// used to derive the key the client sent back, so a key round-trips to
		// the user's own tag instead of creating a near-duplicate of it.
		tag, err = inst.hb.ResolveTag(ctx, category, placement.NormalizeCategory)
		if err != nil {
			// Not fatal: an untagged item is merely invisible to category
			// scoring, whereas refusing to file it loses the capture.
			s.log.Warn("catalog: resolving category tag failed", "category", category, "err", err)
			return nil, placement.MatchCategory("")
		}
		batch.tags[category] = tag
	}
	// The resolved tag's name, not the requested category: ResolveTag matches
	// case-insensitively, so "electronics" can come back as the user's
	// existing "Electronics", and that is the name the rebuild will normalize.
	return []string{tag.ID}, placement.MatchCategory(tag.Name)
}

// containerKey identifies a container by where it goes and what it is called:
// two entries asking for "Tools 1" under the same parent want one container.
func containerKey(nc newContainerRequest) string {
	return strings.Join([]string{nc.Label, nc.ParentID, nc.EntityTypeID}, "\x00")
}

// batchContainer returns the container an entry asked for: the one an earlier
// entry in the SAME request created, the one an EARLIER REQUEST created, or a
// new one.
//
// The middle case is what makes a resend safe. Without it a retried capture
// creates a second container with the same name in the same place, and the
// items split between them.
//
// A container found by key is never re-annotated: a resend after a partial
// failure carries FEWER entries than the request that created it, so anything
// derived from this request's entries would describe less than is inside.
func (s *Server) batchContainer(ctx context.Context, inst *instance, nc newContainerRequest, batch *catalogBatch) (string, error) {
	key := containerKey(nc)
	if id, ok := batch.containers[key]; ok {
		return id, nil
	}
	id := containerKeyID(nc)
	if prior, ok := batch.filed[id]; ok {
		batch.containers[key] = prior.ID
		return prior.ID, nil
	}

	release, waited := inst.claimKey(ctx, id)
	defer release()
	if prior, ok := inst.recentlyFiled(id); ok {
		batch.containers[key] = prior.ID
		return prior.ID, nil
	}
	if waited {
		for k, v := range s.filedByKey(ctx, inst, []string{id}, true) {
			batch.filed[k] = v
		}
		if prior, ok := batch.filed[id]; ok {
			batch.containers[key] = prior.ID
			return prior.ID, nil
		}
	}

	// Size it for everything that will go in it, not for the one entry that
	// happened to ask first.
	box, err := s.createContainer(ctx, inst, nc, id)
	if err != nil {
		return "", err
	}
	inst.rememberFiled(id, box)
	batch.containers[key] = box.ID
	return box.ID, nil
}

// createContainer creates a new box and writes the metadata the engine
// suggested for it.
//
// Without that second step the box we just recommended comes back without the
// placement flag or its access, so the very next recommendation would not
// offer it -- the headline feature would create containers it then has to
// ignore. POST cannot carry fields, hence the separate write.
//
// No capacity is written. It used to be the suggestion's size bucket in units
// (2 for an "M" container), a guess that then read back as a recorded fact
// and excluded the container once two things were in it. A capacity is
// something the user records; until they do, it is unknown.
func (s *Server) createContainer(ctx context.Context, inst *instance, nc newContainerRequest, key string) (homebox.Entity, error) {
	typeID := nc.EntityTypeID
	if typeID == "" {
		// Omitting this auto-resolves a NON-location type, silently creating
		// an item that can never hold anything.
		var err error
		if typeID, err = inst.hb.LocationEntityTypeID(ctx); err != nil {
			return homebox.Entity{}, err
		}
	}
	box, err := inst.hb.CreateEntity(ctx, homebox.CreateEntityRequest{
		Name:         nc.Label,
		ParentID:     nc.ParentID,
		EntityTypeID: typeID,
	})
	if err != nil {
		return homebox.Entity{}, err
	}

	// Accepting a new-container suggestion IS the user saying "put things
	// here", so the container is opted in to placement as it is created.
	// Without this it would be invisible to the engine the moment the cache
	// rebuilt, and the next item from the same shelf would be told to create
	// another one.
	fields := []homebox.CustomField{placementField(true), keyField(key)}
	fields = append(fields, newContainerSize(nc)...)
	if nc.Access != "" {
		fields = append(fields, homebox.CustomField{
			Name: fieldAccess, Type: homebox.FieldTypeText, TextValue: nc.Access,
		})
	}
	// Best effort by design: the item it was created for is filed into it
	// either way, and losing the capture would be the worse failure. What is
	// lost is the metadata -- including the placement flag, so a container
	// this failed on will not be offered for the NEXT item until someone
	// selects it in the location picker.
	if _, err := inst.hb.SetFields(ctx, box.ID, fields); err != nil {
		s.log.Warn("new container created without metadata; it will not be offered for placement until selected",
			"box", box.Name, "err", err)
	}
	return box, nil
}

// itemFields is what makes the box index coherent. Until these land on the
// item, the read path has to guess every category as "other" and every size as
// M, so what the engine reads back bears no relation to what was written.
func itemFields(d placement.ItemDraft) []homebox.CustomField {
	// Category is deliberately absent: it lives as a tag, and duplicating it
	// here would create a second source of truth that drifts the moment
	// someone retags an item in the Homebox UI.
	fields := []homebox.CustomField{
		{Name: fieldSizeBucket, Type: homebox.FieldTypeText, TextValue: d.SizeBucket},
		{Name: fieldWeightClass, Type: homebox.FieldTypeText, TextValue: d.WeightClass},
		{Name: fieldFragile, Type: homebox.FieldTypeBoolean, BooleanValue: d.Fragile},
	}
	// A size only when it was measured: a model's estimate from a photo with
	// no scale in it is not worth recording as a fact about the object.
	if d.DimensionsCm != nil && (d.DimensionsSource == placement.DimsLidar || d.DimensionsSource == placement.DimsManual) {
		fields = append(fields,
			homebox.CustomField{Name: fieldDimensionsCm, Type: homebox.FieldTypeText, TextValue: d.DimensionsCm.String()},
			homebox.CustomField{Name: fieldDimsSource, Type: homebox.FieldTypeText, TextValue: d.DimensionsSource},
		)
	}
	return fields
}

// newContainerSize records the size of a new container when the suggestion
// named one of the user's own types. Its fill starts as a known 0 -- it was
// just made -- marked estimated and with no observation time, so the items
// filed into it in the same request are added (see fillUpdate).
func newContainerSize(nc newContainerRequest) []homebox.CustomField {
	var fields []homebox.CustomField
	if t := strings.TrimSpace(nc.ContainerType); t != "" {
		fields = append(fields, homebox.CustomField{Name: fieldContainerType, Type: homebox.FieldTypeText, TextValue: t})
	}
	if nc.InteriorCm != nil {
		if d, ok := nc.InteriorCm.Normalize(); ok {
			fields = append(fields, homebox.CustomField{Name: fieldInteriorCm, Type: homebox.FieldTypeText, TextValue: d.String()})
		}
	}
	if nc.CapacityL > 0 {
		fields = append(fields, homebox.CustomField{Name: fieldCapacityL, Type: homebox.FieldTypeNumber, NumberValue: float64(nc.CapacityL)})
		fields = append(fields, fillFields(0, placement.FillEstimated, "")...)
	}
	return fields
}

// readCatalogRequest accepts the request in either shape.
//
// The real client sends multipart/form-data: a "payload" part holding the JSON
// and an optional "image" part. One request carrying the items and their photo
// is what lets the offline queue persist a single self-contained unit of work
// rather than several dependent ones that have to be ordered and reconciled
// over bad signal. Plain JSON is still accepted for curl and for tests.
func readCatalogRequest(r *http.Request) (catalogRequest, []byte, string, error) {
	var req catalogRequest

	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data") {
		r.Body = http.MaxBytesReader(nil, r.Body, maxJSONBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return req, nil, "", fmt.Errorf("invalid catalog request: %w", err)
		}
		return req, nil, "", nil
	}

	// ParseMultipartForm's argument is a memory budget, not a size cap: it
	// spills the remainder to temp files. MaxBytesReader is what actually
	// bounds the request.
	r.Body = http.MaxBytesReader(nil, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		return req, nil, "", fmt.Errorf("invalid multipart body: %w", err)
	}

	payload := r.FormValue("payload")
	if payload == "" {
		return req, nil, "", errors.New(`multipart body needs a "payload" part containing the JSON request`)
	}
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		return req, nil, "", fmt.Errorf("invalid payload JSON: %w", err)
	}

	file, header, err := r.FormFile("image")
	if err != nil {
		return req, nil, "", nil // the photo is optional
	}
	defer file.Close()

	data, err := readCapped(file, maxImageBytes)
	if err != nil {
		return req, nil, "", err
	}
	name := filepath.Base(header.Filename)
	if name == "" || name == "." || name == "/" {
		name = "capture.jpg"
	}
	return req, data, name, nil
}

// isNilUUID reports whether id is made of nothing but zeros and hyphens: the
// nil UUID, however it was formatted.
func isNilUUID(id string) bool {
	return id != "" && strings.Trim(id, "0-") == ""
}

// noteFill adds what one landed entry means for its container's fill.
//
// Litres count only for an item CREATED now: a deduped entry was counted when
// it was first filed, and counting it again is exactly how a resend would
// overfill a container. Its observation still counts -- a resend carries the
// same one, so the time check in fillUpdate makes it a no-op when the first
// attempt saved it, and a retry of the write when that attempt did not.
func noteFill(fills map[string]*boxFill, i int, e catalogEntry, res catalogResult, d boxDelta) {
	boxID := d.boxID
	if boxID == "" && res.Entity != nil {
		boxID = res.Entity.ParentID()
	}
	if boxID == "" {
		return
	}
	f := fills[boxID]
	if f == nil {
		f = &boxFill{}
		fills[boxID] = f
	}
	f.entries = append(f.entries, i)
	if !res.Deduped {
		f.litres += d.litres
	}
	if at, ok := e.FillAfter.valid(); ok && (f.observed == nil || at.After(f.observedAt)) {
		obs := *e.FillAfter
		f.observed, f.observedAt = &obs, at
	}
}
