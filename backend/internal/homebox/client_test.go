package homebox

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixture serves a captured response body from testdata/.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// serve stands up a fake Homebox. handler receives every request so tests can
// assert on request construction as well as decoding.
func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL+"/api", "hb_test-token")
}

// The regression test for the defect that made the placement engine useless.
//
// Homebox custom fields are typed: the server returns all three value keys and
// the caller must read the one named by Type. An earlier CustomField mapped
// only textValue, so capacityUnits (a number) read as 0 and fragileSafe (a
// boolean) read as false -- and those are exactly the two fields the engine's
// hard exclusions key off. Verified live: a box holding capacityUnits=8 and
// fragileSafe=true was reported by the backend as 0 and false.
func TestGetEntityDecodesTypedCustomFields(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "entity_detail_location_with_fields.json"))
	})

	e, err := c.GetEntity(context.Background(), "any")
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	f := e.FieldSet()

	if got, ok := f.Number("capacityUnits"); !ok || got != 8 {
		t.Errorf("capacityUnits = %v (ok=%v), want 8, true", got, ok)
	}
	if got, ok := f.Bool("fragileSafe"); !ok || !got {
		t.Errorf("fragileSafe = %v (ok=%v), want true, true", got, ok)
	}
	if got, ok := f.Text("access"); !ok || got != "easy" {
		t.Errorf("access = %q (ok=%v), want \"easy\", true", got, ok)
	}
}

// Accessors are type-checked, so asking for the wrong type reports absence
// rather than a zero value. That distinction is what lets the engine treat an
// unrecorded flag as unknown instead of false.
func TestFieldAccessorsRejectMismatchedTypes(t *testing.T) {
	fs := FieldSet{
		"capacityUnits": {Name: "capacityUnits", Type: FieldTypeNumber, NumberValue: 8},
		"fragileSafe":   {Name: "fragileSafe", Type: FieldTypeBoolean, BooleanValue: true},
	}
	if _, ok := fs.Text("capacityUnits"); ok {
		t.Error("Text() accepted a number field")
	}
	if _, ok := fs.Bool("capacityUnits"); ok {
		t.Error("Bool() accepted a number field")
	}
	if _, ok := fs.Number("fragileSafe"); ok {
		t.Error("Number() accepted a boolean field")
	}
	if _, ok := fs.Number("absent"); ok {
		t.Error("Number() reported an absent field as present")
	}
}

// parentId is write-only. Reads carry a nested parent object and no top-level
// parentId, so a struct decoding parentId would silently see "" everywhere.
func TestParentIsReadFromNestedObject(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "entity_detail_location_with_fields.json"))
	})
	e, err := c.GetEntity(context.Background(), "any")
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if e.ParentName() != "Garage" {
		t.Errorf("ParentName() = %q, want \"Garage\"", e.ParentName())
	}
	if e.ParentID() == "" {
		t.Error("ParentID() is empty; the nested parent object was not decoded")
	}
	if !e.IsLocation() {
		t.Error("IsLocation() = false for an entity whose type has isLocation:true")
	}

	// And the root case must not panic or invent a parent.
	var root Entity
	if root.ParentID() != "" || root.ParentName() != "" || root.IsLocation() {
		t.Error("a zero Entity should report no parent and not be a location")
	}
}

func TestListEntitiesRequestConstruction(t *testing.T) {
	var got url.Values
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		if h := r.Header.Get("Authorization"); h != "Bearer hb_test-token" {
			t.Errorf("Authorization = %q", h)
		}
		w.Write(fixture(t, "entities_empty.json"))
	})

	isLoc := true
	if _, err := c.ListEntities(context.Background(), []string{"p1", "p2"}, &isLoc); err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if ids := got["parentIds"]; len(ids) != 2 || ids[0] != "p1" || ids[1] != "p2" {
		t.Errorf("parentIds = %v, want repeated p1,p2", ids)
	}
	if got.Get("isLocation") != "true" {
		t.Errorf("isLocation = %q, want \"true\"", got.Get("isLocation"))
	}

	// Absent and false are DIFFERENT queries: omitting isLocation returns
	// items only, not everything. Passing nil must send no parameter at all.
	if _, err := c.ListEntities(context.Background(), nil, nil); err != nil {
		t.Fatalf("ListEntities(nil): %v", err)
	}
	if _, present := got["isLocation"]; present {
		t.Error("isLocation was sent when the caller passed nil")
	}
}

// The pair is joined inside the client because `fields=Name` with NO "=" is
// not rejected by Homebox -- it is silently ignored, so a filter made only of
// those returns the entire inventory. A dedupe check reads that as "already
// there" and skips the create; an eligibility check reads it as "everything is
// a candidate". Both are silent, and both are worse than an error.
func TestListEntitiesByFieldRejectsFiltersThatWouldMatchEverything(t *testing.T) {
	sent := 0
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		sent++
		w.Write(fixture(t, "entities_empty.json"))
	})

	bad := []struct {
		name   string
		field  string
		values []string
	}{
		{"empty name", "", []string{"true"}},
		{"no values", "boxwrightPlacement", nil},
		{"empty value", "boxwrightPlacement", []string{""}},
		{"= in the name", "a=b", []string{"true"}},
		{"= in a value", "boxwrightPlacement", []string{"tr=ue"}},
		{"one bad value among good ones", "boxwrightPlacement", []string{"true", ""}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ListEntitiesByField(context.Background(), tc.field, tc.values, nil)
			if !errors.Is(err, ErrFieldFilter) {
				t.Fatalf("err = %v, want ErrFieldFilter", err)
			}
		})
	}
	if sent != 0 {
		t.Errorf("%d requests reached the server; every one of these must be refused before sending", sent)
	}
}

func TestListEntitiesByFieldRequestConstruction(t *testing.T) {
	var got url.Values
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Write(fixture(t, "entities_empty.json"))
	})

	isLoc := true
	if _, err := c.ListEntitiesByField(context.Background(), "boxwrightPlacement", []string{"true", "yes"}, &isLoc); err != nil {
		t.Fatalf("ListEntitiesByField: %v", err)
	}
	// Repeated parameters are OR'd server-side, so several values are one
	// query rather than one query each.
	want := []string{"boxwrightPlacement=true", "boxwrightPlacement=yes"}
	if fields := got["fields"]; len(fields) != 2 || fields[0] != want[0] || fields[1] != want[1] {
		t.Errorf("fields = %v, want %v", fields, want)
	}
	if got.Get("isLocation") != "true" {
		t.Errorf("isLocation = %q", got.Get("isLocation"))
	}
	if got.Get("pageSize") == "" {
		t.Error("the filtered listing does not page; a large selection would be truncated")
	}
}

func TestListEntitiesDecodesCapturedShape(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "entities_locations.json"))
	})
	isLoc := true
	got, err := c.ListEntities(context.Background(), nil, &isLoc)
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d locations, want 10", len(got))
	}
	nested := 0
	for _, e := range got {
		if e.ParentID() != "" {
			nested++
		}
		if len(e.Fields) != 0 {
			t.Errorf("%s: list rows must not carry fields (captured shape has none)", e.Name)
		}
	}
	if nested != 1 {
		t.Errorf("got %d rows with a parent, want 1", nested)
	}
}

func TestListEntitiesPagesToTotal(t *testing.T) {
	const total = 450
	var pages []string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		pages = append(pages, q.Get("page"))
		size := defaultPageSize
		start := (atoi(q.Get("page")) - 1) * size
		items := []Entity{}
		for i := start; i < start+size && i < total; i++ {
			items = append(items, Entity{ID: "e", Name: "e"})
		}
		json.NewEncoder(w).Encode(paginated{Items: items, Total: total})
	})

	got, err := c.ListEntities(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if len(got) != total {
		t.Errorf("collected %d entities, want %d", len(got), total)
	}
	if len(pages) != 3 || pages[0] != "1" || pages[2] != "3" {
		t.Errorf("requested pages %v, want 1,2,3", pages)
	}
}

// A server that ignores paging and returns everything on page 1 must not send
// the client into an infinite loop re-requesting identical responses.
func TestListEntitiesTerminatesWhenServerIgnoresPaging(t *testing.T) {
	calls := 0
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 5 {
			t.Fatal("client looped: server ignores paging and client kept asking")
		}
		json.NewEncoder(w).Encode(paginated{Items: []Entity{{ID: "a"}, {ID: "b"}}, Total: 2})
	})
	got, err := c.ListEntities(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if len(got) != 2 || calls != 1 {
		t.Errorf("got %d entities in %d calls, want 2 in 1", len(got), calls)
	}
}

func TestListEntitiesEmptyCollection(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "entities_empty.json"))
	})
	got, err := c.ListEntities(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("an empty collection must not be an error: %v", err)
	}
	if got == nil {
		t.Error("want an empty non-nil slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("got %d entities, want 0", len(got))
	}
}

// /v1/entity-types returns a bare array, unlike /v1/entities.
func TestLocationEntityTypeID(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/entity-types" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write(fixture(t, "entity_types.json"))
	})
	id, err := c.LocationEntityTypeID(context.Background())
	if err != nil {
		t.Fatalf("LocationEntityTypeID: %v", err)
	}
	if id != "00000002-0000-4000-8000-000000000002" {
		t.Errorf("got %q, want the Location type's id", id)
	}
}

// Creating a location needs a location type. If the group somehow has none,
// that must be a loud error rather than a silent fallback that creates items.
func TestLocationEntityTypeIDErrorsWhenNoneIsALocation(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]EntityType{{ID: "1", Name: "Item", IsLocation: false}})
	})
	if _, err := c.LocationEntityTypeID(context.Background()); err == nil {
		t.Fatal("want an error when no entity type has isLocation=true")
	}
}

// SetFields is the most dangerous call in this package: PUT is a full replace
// against the user's real inventory. An earlier typed implementation was
// measured against a live entity and destroyed purchasePrice, purchaseFrom,
// serialNumber, notes, manufacturer, modelNumber and insured -- every column
// the struct had not modelled. This asserts the passthrough contract: only
// `fields` and `syncChildEntityLocations` may differ between the GET and the
// PUT, and every other key must survive byte-for-byte.
func TestSetFieldsPreservesEveryFieldItDoesNotOwn(t *testing.T) {
	// Shaped like a real art-collection item on the live instance, including
	// a key this client does not model at all.
	const getBody = `{
	  "id": "item-1",
	  "name": "Happy Cow",
	  "description": "Micro Masterpieces 2023",
	  "quantity": 1,
	  "insured": true,
	  "archived": false,
	  "assetId": "000-014",
	  "purchasePrice": 65,
	  "purchaseFrom": "32auctions",
	  "purchaseDate": "2023-12-31",
	  "serialNumber": "137543/4627821",
	  "notes": "Source: https://example.invalid/a",
	  "manufacturer": "Susan Christensen",
	  "modelNumber": "MM-2023",
	  "lifetimeWarranty": false,
	  "someFutureHomeboxField": {"nested": [1, 2, 3]},
	  "createdAt": "2026-01-01T00:00:00Z",
	  "updatedAt": "2026-01-01T00:00:00Z",
	  "totalPrice": 65,
	  "parent": {"id": "row-1", "name": "California"},
	  "entityType": {"id": "loc-type", "name": "Item", "isLocation": false},
	  "tags": [{"id": "tag-1", "name": "MM 2023"}],
	  "fields": [
	    {"id": "f-artist", "name": "Artist", "type": "text", "textValue": "Susan"},
	    {"id": "f-cap", "name": "capacityUnits", "type": "number", "numberValue": 4}
	  ]
	}`

	var put map[string]any
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
				t.Fatalf("decode PUT: %v", err)
			}
			w.Write([]byte(getBody))
			return
		}
		w.Write([]byte(getBody))
	})

	if _, err := c.SetFields(context.Background(), "item-1", []CustomField{
		{Name: "capacityUnits", Type: FieldTypeNumber, NumberValue: 8},
		{Name: "category", Type: FieldTypeText, TextValue: "tools"},
	}); err != nil {
		t.Fatalf("SetFields: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(getBody), &got); err != nil {
		t.Fatal(err)
	}

	// Every scalar the server sent, other than what we deliberately change,
	// must come back unchanged -- including a key this client does not model.
	for _, k := range []string{
		"id", "name", "description", "quantity", "insured", "archived", "assetId",
		"purchasePrice", "purchaseFrom", "purchaseDate", "serialNumber", "notes",
		"manufacturer", "modelNumber", "lifetimeWarranty", "someFutureHomeboxField",
	} {
		if !reflect.DeepEqual(put[k], got[k]) {
			t.Errorf("PUT changed %q: got %#v, want %#v", k, put[k], got[k])
		}
	}

	// Read-only shapes are translated to the ids the PUT expects, not echoed.
	if put["parentId"] != "row-1" {
		t.Errorf("parentId = %#v, want row-1", put["parentId"])
	}
	if put["entityTypeId"] != "loc-type" {
		t.Errorf("entityTypeId = %#v, want loc-type", put["entityTypeId"])
	}
	if ids, _ := put["tagIds"].([]any); len(ids) != 1 || ids[0] != "tag-1" {
		t.Errorf("tagIds = %#v, want [tag-1]", put["tagIds"])
	}
	for _, k := range readOnlyEntityKeys {
		if _, present := put[k]; present {
			t.Errorf("read-only key %q was echoed back to a write endpoint", k)
		}
	}
	if put["syncChildEntityLocations"] != false {
		t.Errorf("syncChildEntityLocations = %#v, want explicit false", put["syncChildEntityLocations"])
	}

	// And the merge itself.
	fields, _ := put["fields"].([]any)
	byName := map[string]map[string]any{}
	for _, f := range fields {
		m := f.(map[string]any)
		byName[m["name"].(string)] = m
	}
	// Artist survives, capacityUnits is replaced in place, category is added.
	if len(byName) != 3 {
		t.Fatalf("got %d fields, want 3 (Artist kept, capacityUnits updated, category added): %v",
			len(byName), byName)
	}
	if byName["Artist"]["textValue"] != "Susan" || byName["Artist"]["id"] != "f-artist" {
		t.Errorf("foreign custom field not preserved: %#v", byName["Artist"])
	}
	if byName["capacityUnits"]["numberValue"] != float64(8) || byName["capacityUnits"]["id"] != "f-cap" {
		t.Errorf("updated field lost its id or value: %#v", byName["capacityUnits"])
	}
	if byName["category"]["textValue"] != "tools" {
		t.Errorf("new field not added: %#v", byName["category"])
	}
}

// Verified against v0.26.2: "file" and "name" are both required, and "type" is
// best left empty so the server infers "photo" from the mime type.
func TestUploadAttachmentSendsRequiredNameField(t *testing.T) {
	var parts map[string]string
	var filename string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("content type: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		parts = map[string]string{}
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			buf := make([]byte, 512)
			n, _ := p.Read(buf)
			parts[p.FormName()] = string(buf[:n])
			if p.FormName() == "file" {
				filename = p.FileName()
			}
		}
		w.WriteHeader(http.StatusCreated)
	})

	if err := c.UploadAttachment(context.Background(), "e1", "capture.jpg", []byte("jpegbytes"), true); err != nil {
		t.Fatalf("UploadAttachment: %v", err)
	}
	if _, ok := parts["file"]; !ok {
		t.Error("missing required part: file")
	}
	if filename != "capture.jpg" {
		t.Errorf("file part filename = %q, want capture.jpg", filename)
	}
	if parts["name"] != "capture.jpg" {
		t.Errorf("name part = %q, want capture.jpg (REQUIRED by the server)", parts["name"])
	}
	if parts["primary"] != "true" {
		t.Errorf("primary = %q, want true", parts["primary"])
	}
	if v, ok := parts["type"]; ok && v != "" {
		t.Errorf("type = %q; leave it empty so the server infers photo from the mime type", v)
	}
}

func TestErrorsCarryStatusAndBody(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusTeapot)
	})
	_, err := c.GetEntity(context.Background(), "e1")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "418") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry status and body, got %q", err)
	}
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// UpdateFields hands the change the fields as they are NOW -- so a fill can be
// added to rather than overwritten -- and writes what it returns through the
// same raw round trip as SetFields.
func TestUpdateFieldsSeesTheCurrentFieldsAndWritesTheChange(t *testing.T) {
	const getBody = `{
	  "id": "tote-1", "name": "Tote 1", "notes": "keep me",
	  "entityType": {"id": "loc-type", "name": "Location", "isLocation": true},
	  "fields": [
	    {"id": "f-fill", "name": "fillPct", "type": "number", "numberValue": 40},
	    {"id": "f-src", "name": "fillSource", "type": "text", "textValue": "observed"}
	  ]
	}`
	var put map[string]any
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
				t.Fatalf("decode PUT: %v", err)
			}
		}
		w.Write([]byte(getBody))
	})

	_, err := c.UpdateFields(context.Background(), "tote-1", func(cur FieldSet) []CustomField {
		fill, ok := cur.Number("fillPct")
		if !ok || fill != 40 {
			t.Errorf("change saw fillPct %v (%v), want the current 40", fill, ok)
		}
		return []CustomField{{Name: "fillPct", Type: FieldTypeNumber, NumberValue: fill + 25}}
	})
	if err != nil {
		t.Fatalf("UpdateFields: %v", err)
	}
	if put["notes"] != "keep me" {
		t.Errorf("notes = %#v: the round trip dropped a column", put["notes"])
	}
	fields, _ := put["fields"].([]any)
	byName := map[string]map[string]any{}
	for _, f := range fields {
		m, _ := f.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if got := byName["fillPct"]; got["numberValue"] != 65.0 || got["id"] != "f-fill" {
		t.Errorf("fillPct written as %#v, want 65 keeping its id", got)
	}
	if got := byName["fillSource"]; got["textValue"] != "observed" {
		t.Errorf("fillSource = %#v, want it left alone", got)
	}
}

// A change with nothing to say writes nothing: no PUT, no bumped updatedAt,
// no echo down the change feed.
func TestUpdateFieldsWithNothingToChangeDoesNotWrite(t *testing.T) {
	puts := 0
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
		}
		w.Write([]byte(`{"id": "tote-1", "name": "Tote 1", "fields": []}`))
	})
	got, err := c.UpdateFields(context.Background(), "tote-1", func(FieldSet) []CustomField { return nil })
	if err != nil {
		t.Fatalf("UpdateFields: %v", err)
	}
	if puts != 0 {
		t.Errorf("%d PUTs for a change that returned nothing", puts)
	}
	if got.ID != "tote-1" {
		t.Errorf("returned %+v, want the entity as read", got)
	}
}
