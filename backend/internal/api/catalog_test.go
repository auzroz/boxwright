package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"boxwright/internal/homebox"
	"boxwright/internal/placement"
)

// recordingHomebox captures every write so the tests can assert on the exact
// sequence of calls the catalog path makes.
type recordingHomebox struct {
	created     []homebox.CreateEntityRequest
	fieldWrites map[string][]homebox.CustomField
	attachments []struct {
		entityID, name string
		size           int
		primary        bool
	}
	tagsCreated  []string
	attachFails  bool
	fieldsFail   bool
	nextID       int
	existingTags []homebox.Tag
	// createFails names an item whose creation blows up, so a test can watch
	// one entry of several fail without taking the others down with it.
	createFails string
}

// oneCatalog is the single-entry request most of these tests exercise. The
// wire is an array now, but one item in one photo is still the common case.
func oneCatalog(item placement.ItemDraft, boxID string) catalogRequest {
	return catalogRequest{Entries: []catalogEntry{{Item: item, BoxID: boxID}}}
}

// catalogResults decodes the per-entry results. They are positional:
// results[i] is entries[i], landed or not.
func catalogResults(t *testing.T, body io.Reader) []map[string]any {
	t.Helper()
	var out struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Results
}

// postMultipartCatalog runs one request and returns the recorder.
func postMultipartCatalog(t *testing.T, s *Server, payload any, image []byte, filename string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartCatalog(t, payload, image, filename)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/catalog", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	return rr
}

func (f *recordingHomebox) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tags":
			json.NewEncoder(w).Encode(f.existingTags)

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tags":
			var req homebox.TagCreateRequest
			json.NewDecoder(r.Body).Decode(&req)
			f.tagsCreated = append(f.tagsCreated, req.Name)
			t := homebox.Tag{ID: "tag-" + req.Name, Name: req.Name}
			f.existingTags = append(f.existingTags, t)
			json.NewEncoder(w).Encode(t)

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/entity-types":
			json.NewEncoder(w).Encode([]homebox.EntityType{
				{ID: "item-type", Name: "Item"},
				{ID: "loc-type", Name: "Location", IsLocation: true},
			})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attachments"):
			if f.attachFails {
				http.Error(w, "storage full", http.StatusInsufficientStorage)
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/entities/"), "/attachments")
			r.ParseMultipartForm(1 << 20)
			file, hdr, err := r.FormFile("file")
			if err != nil {
				http.Error(w, "no file", http.StatusBadRequest)
				return
			}
			defer file.Close()
			body, _ := io.ReadAll(file)
			primary := r.FormValue("primary") == "true"
			name := r.FormValue("name")
			if name == "" {
				http.Error(w, "name is required", http.StatusUnprocessableEntity)
				return
			}
			_ = hdr
			f.attachments = append(f.attachments, struct {
				entityID, name string
				size           int
				primary        bool
			}{id, name, len(body), primary})
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/entities":
			var req homebox.CreateEntityRequest
			json.NewDecoder(r.Body).Decode(&req)
			if f.createFails != "" && req.Name == f.createFails {
				http.Error(w, "conflict", http.StatusConflict)
				return
			}
			f.created = append(f.created, req)
			f.nextID++
			json.NewEncoder(w).Encode(map[string]any{
				"id": "e" + string(rune('0'+f.nextID)), "name": req.Name,
			})

		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/entities/"):
			if f.fieldsFail {
				http.Error(w, "nope", http.StatusInternalServerError)
				return
			}
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/entities/")
			var body struct {
				Fields []homebox.CustomField `json:"fields"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if f.fieldWrites == nil {
				f.fieldWrites = map[string][]homebox.CustomField{}
			}
			f.fieldWrites[id] = body.Fields
			json.NewEncoder(w).Encode(map[string]any{"id": id})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/entities/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/entities/")
			json.NewEncoder(w).Encode(map[string]any{"id": id, "name": id, "fields": []any{}})

		default:
			json.NewEncoder(w).Encode(page())
		}
	}
}

func catalogServer(t *testing.T, f *recordingHomebox) *Server {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return New(homebox.New(srv.URL+"/api", "tok"), mustProvider(t), time.Minute, discardLogger())
}

func multipartCatalog(t *testing.T, payload any, image []byte, filename string) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	b, _ := json.Marshal(payload)
	w.WriteField("payload", string(b))
	if image != nil {
		fw, err := w.CreateFormFile("image", filename)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(image)
	}
	w.Close()
	return &body, w.FormDataContentType()
}

func TestCatalogMultipartFilesItemAndUploadsPhoto(t *testing.T) {
	f := &recordingHomebox{existingTags: []homebox.Tag{{ID: "tag-elec", Name: "Electronics"}}}
	s := catalogServer(t, f)

	rr := postMultipartCatalog(t, s, oneCatalog(placement.ItemDraft{
		Name: "cordless drill", Category: "tools", SizeBucket: "M",
		WeightClass: "heavy", Fragile: false, Quantity: 3, Notes: "DeWalt",
	}, "fresno"), []byte("jpegbytes"), "capture.jpg")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	results := catalogResults(t, rr.Body)
	if len(results) != 1 {
		t.Fatalf("got %d results, want one per entry", len(results))
	}
	if results[0]["photoUploaded"] != true {
		t.Errorf("photoUploaded = %v, want true", results[0]["photoUploaded"])
	}
	if results[0]["fieldsWritten"] != true {
		t.Errorf("fieldsWritten = %v, want true", results[0]["fieldsWritten"])
	}

	if len(f.created) != 1 {
		t.Fatalf("created %d entities, want 1", len(f.created))
	}
	got := f.created[0]
	if got.ParentID != "fresno" {
		t.Errorf("parentId = %q, want fresno", got.ParentID)
	}
	if got.Quantity != 3 {
		t.Errorf("quantity = %d, want 3 (twelve mason jars must not need twelve photos)", got.Quantity)
	}
	if len(got.TagIDs) != 1 {
		t.Errorf("tagIds = %v, want the category tag", got.TagIDs)
	}
	// "tools" did not exist, so it is created -- title-cased to sit alongside
	// the user's own "Electronics" rather than beside it in lower case.
	if len(f.tagsCreated) != 1 || f.tagsCreated[0] != "Tools" {
		t.Errorf("tags created = %v, want [Tools]", f.tagsCreated)
	}
	if len(f.attachments) != 1 {
		t.Fatalf("uploaded %d attachments, want 1", len(f.attachments))
	}
	if a := f.attachments[0]; a.name != "capture.jpg" || !a.primary || a.size != 9 {
		t.Errorf("attachment = %+v, want capture.jpg, primary, 9 bytes", a)
	}
}

// An existing tag must be reused whatever its case, or the user's tag list
// grows a lowercase twin of every category.
func TestCatalogReusesExistingTagCaseInsensitively(t *testing.T) {
	f := &recordingHomebox{existingTags: []homebox.Tag{{ID: "tag-elec", Name: "Electronics"}}}
	s := catalogServer(t, f)

	rr := postMultipartCatalog(t, s, oneCatalog(
		placement.ItemDraft{Name: "router", Category: "electronics", SizeBucket: "S"}, "fresno"), nil, "")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if len(f.tagsCreated) != 0 {
		t.Errorf("created tags %v; Electronics already existed", f.tagsCreated)
	}
	if len(f.created) != 1 || len(f.created[0].TagIDs) != 1 || f.created[0].TagIDs[0] != "tag-elec" {
		t.Errorf("did not reuse the existing tag: %+v", f.created)
	}
}

// The item landing in the system of record is the durable outcome. Losing it
// because an attachment failed would invert the product's priorities.
func TestCatalogSucceedsWhenPhotoUploadFails(t *testing.T) {
	f := &recordingHomebox{attachFails: true}
	s := catalogServer(t, f)

	rr := postMultipartCatalog(t, s, oneCatalog(
		placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"}, "fresno"),
		[]byte("jpeg"), "c.jpg")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: the item was filed", rr.Code)
	}
	results := catalogResults(t, rr.Body)
	if results[0]["photoUploaded"] != false {
		t.Error("photoUploaded should be false")
	}
	if results[0]["photoError"] == nil {
		t.Error("the client needs to know the photo did not stick, so it can offer a retry")
	}
	if len(f.created) != 1 {
		t.Error("the item itself should still have been created")
	}
}

func TestCatalogSucceedsWhenFieldWriteFails(t *testing.T) {
	f := &recordingHomebox{fieldsFail: true}
	s := catalogServer(t, f)

	rr := postMultipartCatalog(t, s, oneCatalog(
		placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"}, "fresno"), nil, "")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201", rr.Code)
	}
	if catalogResults(t, rr.Body)[0]["fieldsWritten"] != false {
		t.Error("fieldsWritten should report the failure")
	}
}

// The headline feature. A container created because nothing fitted must come
// back with capacity and access recorded, or the next recommendation cannot
// score the box we just told the user to make.
func TestNewContainerIsCreatedAsALocationAndAnnotated(t *testing.T) {
	f := &recordingHomebox{}
	s := catalogServer(t, f)

	rr := postMultipartCatalog(t, s, catalogRequest{Entries: []catalogEntry{{
		Item: placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"},
		NewContainer: &newContainerRequest{
			Label: "Tools 1", ParentID: "california", SizeBucket: "L", Access: "easy",
		},
	}}}, nil, "")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if len(f.created) != 2 {
		t.Fatalf("created %d entities, want 2 (the box then the item)", len(f.created))
	}
	box := f.created[0]
	if box.Name != "Tools 1" || box.ParentID != "california" {
		t.Errorf("box = %+v", box)
	}
	// Without an explicit location type the server auto-resolves a
	// NON-location type and the "box" is silently an item.
	if box.EntityTypeID != "loc-type" {
		t.Errorf("box entityTypeId = %q, want the location type", box.EntityTypeID)
	}
	if f.created[1].ParentID != "e1" {
		t.Errorf("item was not filed into the new box: parent %q", f.created[1].ParentID)
	}

	meta := f.fieldWrites["e1"]
	byName := map[string]homebox.CustomField{}
	for _, m := range meta {
		byName[m.Name] = m
	}
	// No capacity: a size bucket is a guess, and a guess written as a
	// capacity read back as a fact that excluded the container at two items.
	if got, ok := byName["capacityUnits"]; ok {
		t.Errorf("capacityUnits written (%v); a new container's capacity is unknown until the user records it", got.NumberValue)
	}
	if got := byName["boxwrightPlacement"]; got.TextValue != "true" {
		t.Errorf("boxwrightPlacement = %q, want the new container opted in to placement", got.TextValue)
	}
	if got := byName["access"]; got.TextValue != "easy" {
		t.Errorf("access = %q, want easy", got.TextValue)
	}
}

// An oversized capture must be refused, not silently truncated into a corrupt
// JPEG that is then handed to a vision model and to Homebox with a 200.
func TestOversizedImageIsRejectedNotTruncated(t *testing.T) {
	f := &recordingHomebox{}
	s := catalogServer(t, f)

	big := bytes.Repeat([]byte("x"), maxImageBytes+1024)
	rr := postMultipartCatalog(t, s, oneCatalog(
		placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"}, "fresno"),
		big, "huge.jpg")

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413: %s", rr.Code, rr.Body)
	}
	if len(f.created) != 0 {
		t.Error("an item was filed despite the oversized photo")
	}
}

// Plain JSON stays supported for curl and for anything that has no photo.
func TestCatalogStillAcceptsPlainJSON(t *testing.T) {
	f := &recordingHomebox{}
	s := catalogServer(t, f)

	b, _ := json.Marshal(oneCatalog(
		placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"}, "fresno"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/catalog", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if len(f.created) != 1 {
		t.Errorf("created %d, want 1", len(f.created))
	}
}

// Boxwright must never write its own vocabulary into the user's Homebox. The
// category the user types is resolved against their EXISTING tags; an alias
// firing on the write path would fold "Books" to "books-media", find no match,
// and create a near-duplicate "Books Media" beside their own tag -- after
// which every book they catalogue is invisible under the filter they already
// use. Aliases belong at comparison time, not storage time.
func TestCatalogNeverCreatesATwinOfAUsersExistingTag(t *testing.T) {
	for _, tc := range []struct{ typed, existing string }{
		{"Books", "Books"},
		{"books", "Books"},
		{"Toys", "Toys"},
		{"Sports", "Sports"},
		{"Misc", "Misc"},
	} {
		t.Run(tc.typed, func(t *testing.T) {
			f := &recordingHomebox{existingTags: []homebox.Tag{{ID: "t1", Name: tc.existing}}}
			s := catalogServer(t, f)

			rr := postMultipartCatalog(t, s, oneCatalog(
				placement.ItemDraft{Name: "a thing", Category: tc.typed, SizeBucket: "M"}, "fresno"), nil, "")

			if rr.Code != http.StatusCreated {
				t.Fatalf("status %d: %s", rr.Code, rr.Body)
			}
			if len(f.tagsCreated) != 0 {
				t.Errorf("created tag(s) %v; %q already exists and must be reused",
					f.tagsCreated, tc.existing)
			}
			if len(f.created) != 1 || len(f.created[0].TagIDs) != 1 || f.created[0].TagIDs[0] != "t1" {
				t.Errorf("did not attach the existing tag: %+v", f.created)
			}
		})
	}
}

// The key GET /api/v1/categories hands the app must round-trip back through
// /catalog to the SAME tag. The endpoint derives a key by shape-folding the tag
// name, so the user's "Micro Masterpieces" is offered as "micro-masterpieces".
// Comparing case only, as ResolveTag once did, does not match those, so filing
// under it created a second tag and detached every newly filed item from the
// 132-piece collection the user already has.
func TestCategoryKeyRoundTripsToTheUsersOwnTag(t *testing.T) {
	for _, existing := range []string{
		"Micro Masterpieces", // multi-word: the case the old comparison missed
		"MM 2026",
		"Electronics",
		"IOT",
	} {
		t.Run(existing, func(t *testing.T) {
			f := &recordingHomebox{existingTags: []homebox.Tag{{ID: "t1", Name: existing}}}
			s := catalogServer(t, f)

			// Exactly what the categories endpoint would have offered.
			key := placement.NormalizeCategory(existing)

			rr := postMultipartCatalog(t, s, oneCatalog(
				placement.ItemDraft{Name: "a thing", Category: key, SizeBucket: "M"}, "fresno"), nil, "")

			if rr.Code != http.StatusCreated {
				t.Fatalf("status %d: %s", rr.Code, rr.Body)
			}
			if len(f.tagsCreated) != 0 {
				t.Errorf("key %q created tag(s) %v; it must resolve to the existing %q",
					key, f.tagsCreated, existing)
			}
			if len(f.created) != 1 || len(f.created[0].TagIDs) != 1 || f.created[0].TagIDs[0] != "t1" {
				t.Errorf("did not attach the existing tag: %+v", f.created)
			}
		})
	}
}

// One photo, N entities. Every item from a shelf shot has to reach Homebox,
// each with its own destination -- two items out of one photo may legitimately
// belong in different totes -- and the one photo has to end up on every one of
// them, because a Homebox attachment belongs to a single entity and an item
// with no picture is most of the way to an item nobody can find later.
func TestCatalogFilesEveryEntryFromOnePhoto(t *testing.T) {
	f := &recordingHomebox{}
	s := catalogServer(t, f)

	rr := postMultipartCatalog(t, s, catalogRequest{
		CaptureID: "capture-1",
		Entries: []catalogEntry{
			{Item: placement.ItemDraft{Name: "cordless drill", Category: "tools", SizeBucket: "M"}, BoxID: "fresno"},
			{Item: placement.ItemDraft{Name: "tape measure", Category: "tools", SizeBucket: "S"}, BoxID: "fresno"},
			{Item: placement.ItemDraft{Name: "mason jar", Category: "kitchen", SizeBucket: "S", Quantity: 12}, BoxID: "modesto"},
		},
	}, []byte("jpegbytes"), "shelf.jpg")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	results := catalogResults(t, rr.Body)
	if len(results) != 3 {
		t.Fatalf("got %d results, want one per entry", len(results))
	}
	for i, res := range results {
		if res["error"] != nil {
			t.Errorf("results[%d] failed: %v", i, res["error"])
		}
		if res["entity"] == nil {
			t.Errorf("results[%d] has no entity", i)
		}
		if res["photoUploaded"] != true {
			t.Errorf("results[%d] photoUploaded = %v; the capture belongs on every item in it",
				i, res["photoUploaded"])
		}
	}

	if len(f.created) != 3 {
		t.Fatalf("created %d entities, want 3", len(f.created))
	}
	// Destinations are positional, not best effort.
	if f.created[0].Name != "cordless drill" || f.created[0].ParentID != "fresno" {
		t.Errorf("entry 0 = %+v", f.created[0])
	}
	if f.created[2].Name != "mason jar" || f.created[2].ParentID != "modesto" {
		t.Errorf("entry 2 = %+v", f.created[2])
	}
	if f.created[2].Quantity != 12 {
		t.Errorf("quantity = %d, want 12: twelve jars are one entry, not twelve", f.created[2].Quantity)
	}
	if len(f.attachments) != 3 {
		t.Fatalf("uploaded %d attachments, want one per entity: %+v", len(f.attachments), f.attachments)
	}
	// Two entries share the tools category: it is resolved once and the same
	// tag reused, never created twice.
	if len(f.tagsCreated) != 2 {
		t.Errorf("tags created = %v, want exactly Tools and Kitchen", f.tagsCreated)
	}
	if f.created[0].TagIDs[0] != f.created[1].TagIDs[0] {
		t.Errorf("two tools items got different tags: %v vs %v", f.created[0].TagIDs, f.created[1].TagIDs)
	}
}

// The rule the per-entry results exist for: one entry failing must not roll
// back, abort or hide the others. Losing seven filed items to report the
// eighth's failure would be exactly the silent loss this change ends.
func TestCatalogPartialFailureKeepsTheEntriesThatLanded(t *testing.T) {
	f := &recordingHomebox{createFails: "tape measure"}
	s := catalogServer(t, f)

	rr := postMultipartCatalog(t, s, catalogRequest{Entries: []catalogEntry{
		{Item: placement.ItemDraft{Name: "cordless drill", Category: "tools", SizeBucket: "M"}, BoxID: "fresno"},
		{Item: placement.ItemDraft{Name: "tape measure", Category: "tools", SizeBucket: "S"}, BoxID: "fresno"},
		{Item: placement.ItemDraft{Name: "spirit level", Category: "tools", SizeBucket: "L"}, BoxID: "fresno"},
		{Item: placement.ItemDraft{Name: "", Category: "tools", SizeBucket: "S"}, BoxID: "fresno"},
	}}, []byte("jpeg"), "shelf.jpg")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201: three of four entries landed: %s", rr.Code, rr.Body)
	}
	results := catalogResults(t, rr.Body)
	if len(results) != 4 {
		t.Fatalf("got %d results, want one per entry even for the failures", len(results))
	}
	for _, i := range []int{0, 2} {
		if results[i]["error"] != nil || results[i]["entity"] == nil {
			t.Errorf("results[%d] = %v; a neighbour's failure took it down", i, results[i])
		}
	}
	if results[1]["error"] == nil {
		t.Error("the failed entry reported no error, so the client cannot retry it")
	}
	if results[1]["entity"] != nil {
		t.Errorf("results[1].entity = %v, want null: nothing was created", results[1]["entity"])
	}
	// A malformed entry is reported the same way, and stops nothing.
	if results[3]["error"] == nil {
		t.Error("an entry with no name should report why it was refused")
	}
	if len(f.created) != 2 {
		t.Errorf("created %d entities, want the two that could be: %+v", len(f.created), f.created)
	}
}

// Nothing landed and nothing was even sent upstream: that is the client's
// request being wrong, not Homebox failing, and the status has to say so.
func TestCatalogWithOnlyMalformedEntriesIsABadRequest(t *testing.T) {
	f := &recordingHomebox{}
	s := catalogServer(t, f)

	rr := postMultipartCatalog(t, s, catalogRequest{Entries: []catalogEntry{
		{Item: placement.ItemDraft{Name: "", SizeBucket: "M"}, BoxID: "fresno"},
		{Item: placement.ItemDraft{Name: "drill", SizeBucket: "M"}}, // no destination
	}}, nil, "")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rr.Code, rr.Body)
	}
	results := catalogResults(t, rr.Body)
	if len(results) != 2 || results[0]["error"] == nil || results[1]["error"] == nil {
		t.Errorf("both entries should report their own reason: %v", results)
	}
	if len(f.created) != 0 {
		t.Errorf("created %+v from entries that could not be filed", f.created)
	}
}

// Homebox v0.26.2 files an item whose parentId is the nil UUID at the TOP
// LEVEL and answers 201 (a parentId that merely does not exist is a 404). So
// the nil UUID has to be refused here, or the entry reports success from a
// place the user never chose.
func TestCatalogRefusesTheNilUUIDAsABox(t *testing.T) {
	for _, box := range []string{"00000000-0000-0000-0000-000000000000", "00000000000000000000000000000000"} {
		t.Run(box, func(t *testing.T) {
			f := &recordingHomebox{}
			s := catalogServer(t, f)

			rr := postMultipartCatalog(t, s, catalogRequest{Entries: []catalogEntry{
				{Item: placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"}, BoxID: box},
			}}, nil, "")

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rr.Code, rr.Body)
			}
			if len(f.created) != 0 {
				t.Errorf("created %+v under the nil UUID", f.created)
			}
		})
	}
}

func TestCatalogWithNoEntriesIsRefused(t *testing.T) {
	f := &recordingHomebox{}
	s := catalogServer(t, f)

	if rr := postMultipartCatalog(t, s, catalogRequest{CaptureID: "c1"}, nil, ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rr.Code, rr.Body)
	}
}

// Two items from one photo that both need a new tools tote mean ONE tote. Left
// alone, each entry creates its own, and the row ends up holding two boxes with
// the same name -- with the second item filed away from the first for no reason
// the user could ever see.
func TestCatalogCreatesOneContainerForEntriesThatAskForTheSameOne(t *testing.T) {
	f := &recordingHomebox{}
	s := catalogServer(t, f)

	nc := func() *newContainerRequest {
		return &newContainerRequest{Label: "Tools 1", ParentID: "california", SizeBucket: "L", Access: "easy"}
	}
	rr := postMultipartCatalog(t, s, catalogRequest{Entries: []catalogEntry{
		{Item: placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"}, NewContainer: nc()},
		{Item: placement.ItemDraft{Name: "spirit level", Category: "tools", SizeBucket: "L"}, NewContainer: nc()},
	}}, nil, "")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	boxes := 0
	for _, c := range f.created {
		if c.EntityTypeID == "loc-type" {
			boxes++
		}
	}
	if boxes != 1 {
		t.Fatalf("created %d containers, want 1 for two entries naming the same box: %+v", boxes, f.created)
	}
	if len(f.created) != 3 {
		t.Fatalf("created %d entities, want the box then both items: %+v", len(f.created), f.created)
	}
	if f.created[1].ParentID != f.created[2].ParentID {
		t.Errorf("the two items landed in different boxes: %q and %q",
			f.created[1].ParentID, f.created[2].ParentID)
	}
}

// Five items falling back to one suggested container get ONE container, and it
// is given no capacity. It used to be sized from the suggestion's bucket, which
// is a guess about one item: five M tools produced an L box of 4 units holding
// 10, excluded by the capacity rule from every future item. A box the product
// just told the user to create must not be dead on arrival.
func TestSharedNewContainerIsCreatedOnceAndNeverGivenACapacity(t *testing.T) {
	f := &recordingHomebox{}
	s := catalogServer(t, f)

	const n = 5
	entries := make([]catalogEntry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, catalogEntry{
			Item: placement.ItemDraft{
				Name: fmt.Sprintf("tool %d", i), Category: "tools",
				SizeBucket: "M", Quantity: 1,
			},
			NewContainer: &newContainerRequest{
				Label: "Tools 1", ParentID: "california", SizeBucket: "L", Access: "easy",
			},
		})
	}

	body, ct := multipartCatalog(t, catalogRequest{Entries: entries}, nil, "")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/catalog", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}

	// One shared container, not five.
	boxes := 0
	var boxID string
	for _, c := range f.created {
		if c.Name == "Tools 1" {
			boxes++
			boxID = "e1"
		}
	}
	if boxes != 1 {
		t.Fatalf("created %d containers named Tools 1, want 1", boxes)
	}

	for _, fld := range f.fieldWrites[boxID] {
		if fld.Name == "capacityUnits" {
			t.Errorf("capacityUnits = %v written; a created container's capacity is unknown, "+
				"and an unknown capacity never excludes it", fld.NumberValue)
		}
	}
}
