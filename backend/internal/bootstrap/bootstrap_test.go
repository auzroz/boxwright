package bootstrap

import (
	"context"
	"strings"
	"testing"

	"boxwright/internal/homebox"
)

func parse(t *testing.T, s string) []*Node {
	t.Helper()
	roots, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return roots
}

func TestParseOutline(t *testing.T) {
	roots := parse(t, `
# a comment
Garage
  California
    Fresno        access=easy heavySafe=true capacity=12
    Modesto
  Nevada
    Reno

Basement
  Hall Closet     access=deep
`)
	if len(roots) != 2 {
		t.Fatalf("got %d roots, want 2", len(roots))
	}
	garage := roots[0]
	if garage.Name != "Garage" || len(garage.Children) != 2 {
		t.Fatalf("garage = %+v", garage)
	}
	fresno := garage.Children[0].Children[0]
	if fresno.Name != "Fresno" || fresno.Access != "easy" {
		t.Errorf("fresno = %+v", fresno)
	}
	if fresno.HeavySafe == nil || !*fresno.HeavySafe {
		t.Errorf("heavySafe not parsed: %+v", fresno.HeavySafe)
	}
	if fresno.CapacityUnits == nil || *fresno.CapacityUnits != 12 {
		t.Errorf("capacity not parsed: %+v", fresno.CapacityUnits)
	}
	// Multi-word names must survive, since rooms are called things like
	// "Hall Closet" and totes could be "San Luis Obispo".
	if got := roots[1].Children[0]; got.Name != "Hall Closet" || got.Access != "deep" {
		t.Errorf("multi-word name mishandled: %+v", got)
	}
	// Leaves hold items; rows and rooms do not.
	if garage.Leaf() || !fresno.Leaf() {
		t.Error("Leaf() is wrong for a room or a tote")
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"tabs", "Garage\n\tCalifornia\n"},
		{"odd indent", "Garage\n   California\n"},
		{"skipped level", "Garage\n    Fresno\n"},
		{"unknown setting", "Garage\n  Fresno color=blue\n"},
		{"bad access", "Garage\n  Fresno access=sideways\n"},
		{"bad capacity", "Garage\n  Fresno capacity=zero\n"},
		{"negative capacity", "Garage\n  Fresno capacity=-3\n"},
		{"bad bool", "Garage\n  Fresno heavySafe=yes\n"},
		{"name after settings", "Garage\n  Fresno access=easy Extra\n"},
		{"empty", "\n# nothing\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(tc.in)); err == nil {
				t.Errorf("want an error for %q", tc.in)
			}
		})
	}
}

// Unset flags must stay unset. Defaulting heavySafe to false would exclude the
// tote from every heavy item forever, which is the cold-start trap.
func TestMetadataOmitsUnsetFlags(t *testing.T) {
	roots := parse(t, "Garage\n  Bare\n")
	meta := roots[0].Children[0].Metadata(8, "normal", true)

	names := map[string]homebox.CustomField{}
	for _, f := range meta {
		names[f.Name] = f
	}
	if _, present := names["heavySafe"]; present {
		t.Error("heavySafe was written despite not being specified")
	}
	if _, present := names["fragileSafe"]; present {
		t.Error("fragileSafe was written despite not being specified")
	}
	if got := names["capacityUnits"]; got.NumberValue != 8 || got.Type != homebox.FieldTypeNumber {
		t.Errorf("capacityUnits = %+v, want the default 8 as a number", got)
	}
	if got := names["access"]; got.TextValue != "normal" || got.Type != homebox.FieldTypeText {
		t.Errorf("access = %+v, want the default \"normal\" as text", got)
	}

	// Rows and rooms are not placement candidates and get nothing.
	if meta := roots[0].Metadata(8, "normal", true); meta != nil {
		t.Errorf("a room should get no metadata, got %+v", meta)
	}
}

// fakeClient records what Run does.
type fakeClient struct {
	existing  []homebox.Entity
	created   []homebox.CreateEntityRequest
	annotated map[string][]homebox.CustomField
	nextID    int
	noLocType bool
}

func (f *fakeClient) ListEntities(ctx context.Context, parentIDs []string, isLocation *bool) ([]homebox.Entity, error) {
	return f.existing, nil
}
func (f *fakeClient) CreateEntity(ctx context.Context, req homebox.CreateEntityRequest) (homebox.Entity, error) {
	f.created = append(f.created, req)
	f.nextID++
	id := string(rune('a' + f.nextID))
	e := homebox.Entity{ID: id, Name: req.Name}
	if req.ParentID != "" {
		e.Parent = &homebox.EntityRef{ID: req.ParentID}
	}
	f.existing = append(f.existing, e)
	return e, nil
}
func (f *fakeClient) SetFields(ctx context.Context, id string, want []homebox.CustomField) (homebox.Entity, error) {
	if f.annotated == nil {
		f.annotated = map[string][]homebox.CustomField{}
	}
	f.annotated[id] = want
	return homebox.Entity{ID: id}, nil
}
func (f *fakeClient) LocationEntityTypeID(ctx context.Context) (string, error) {
	if f.noLocType {
		return "", context.DeadlineExceeded
	}
	return "loc-type", nil
}

func TestApplyCreatesLocationsWithTheLocationType(t *testing.T) {
	f := &fakeClient{}
	roots := parse(t, "Garage\n  California\n    Fresno access=easy\n")

	if _, err := Run(context.Background(), f, roots, Options{DefaultCapacity: 8, Apply: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.created) != 3 {
		t.Fatalf("created %d, want 3", len(f.created))
	}
	for _, c := range f.created {
		// Omitting this auto-resolves a NON-location type, silently creating
		// items that can never hold anything.
		if c.EntityTypeID != "loc-type" {
			t.Errorf("%q created without the location entity type", c.Name)
		}
	}
	if f.created[0].ParentID != "" {
		t.Errorf("Garage should be top level, got parent %q", f.created[0].ParentID)
	}
	if f.created[1].ParentID == "" || f.created[2].ParentID == "" {
		t.Error("nested locations were created without a parent")
	}
	// Only the leaf is annotated.
	if len(f.annotated) != 1 {
		t.Errorf("annotated %d locations, want 1 (the tote only): %v", len(f.annotated), f.annotated)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	f := &fakeClient{existing: []homebox.Entity{
		{ID: "g", Name: "Garage"},
		{ID: "c", Name: "california", Parent: &homebox.EntityRef{ID: "g"}}, // different case on purpose
	}}
	roots := parse(t, "Garage\n  California\n    Fresno\n")

	actions, err := Run(context.Background(), f, roots, Options{DefaultCapacity: 8, Apply: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.created) != 1 || f.created[0].Name != "Fresno" {
		t.Fatalf("created %+v, want only Fresno", f.created)
	}
	if actions[0].Create || actions[1].Create {
		t.Error("an existing location was reported as a create")
	}

	// Re-running must be a no-op.
	before := len(f.created)
	if _, err := Run(context.Background(), f, roots, Options{DefaultCapacity: 8, Apply: true}); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(f.created) != before {
		t.Errorf("re-running created %d more locations; it must be idempotent", len(f.created)-before)
	}
}

// A dry run must not resolve a child against an empty parent id, or an
// unrelated top-level location of the same name reads as "already exists".
func TestDryRunDoesNotFalseMatchAcrossParents(t *testing.T) {
	f := &fakeClient{existing: []homebox.Entity{
		{ID: "top", Name: "Fresno"}, // a top-level location that happens to share the name
	}}
	roots := parse(t, "Garage\n  California\n    Fresno\n")

	actions, err := Run(context.Background(), f, roots, Options{DefaultCapacity: 8})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.created) != 0 {
		t.Error("a dry run wrote to Homebox")
	}
	for _, a := range actions {
		if !a.Create {
			t.Errorf("%q reported as existing; nothing under Garage exists yet", a.Path)
		}
	}
}

// Refuse rather than corrupt: without a location entity type, every "location"
// created would silently be an item.
func TestApplyRefusesWithoutALocationEntityType(t *testing.T) {
	f := &fakeClient{noLocType: true}
	roots := parse(t, "Garage\n  Fresno\n")

	if _, err := Run(context.Background(), f, roots, Options{Apply: true}); err == nil {
		t.Fatal("want an error when no location entity type exists")
	}
	if len(f.created) != 0 {
		t.Error("created locations despite having no location type")
	}
}

// Homebox is the system of record. An outline that says nothing about access
// must not undo a value the user set in the Homebox UI -- otherwise every
// re-run silently resets their tuning to the CLI's defaults.
func TestRerunDoesNotOverwriteUserTunedMetadata(t *testing.T) {
	roots := parse(t, "Garage\n  Fresno\n")
	leaf := roots[0].Children[0]

	creating := leaf.Metadata(8, "normal", true)
	if len(creating) != 3 {
		t.Fatalf("on create want the placement flag plus capacity+access defaults, got %+v", creating)
	}

	existing := leaf.Metadata(8, "normal", false)
	if len(existing) != 0 {
		t.Errorf("a re-run over an existing box wrote %+v; the outline says nothing "+
			"about these, so Homebox's values must stand", existing)
	}
}

// Creating a location from an outline is the user declaring it a place to put
// things, so it is opted in to automated placement. Re-running is not: turning
// one off in the picker is a decision, and a silent outline must not undo it.
func TestBootstrapMarksNewLeavesEligible(t *testing.T) {
	leaf := parse(t, "Garage\n  Fresno\n")[0].Children[0]

	find := func(fields []homebox.CustomField) (homebox.CustomField, bool) {
		for _, f := range fields {
			if f.Name == "boxwrightPlacement" {
				return f, true
			}
		}
		return homebox.CustomField{}, false
	}

	got, ok := find(leaf.Metadata(8, "normal", true))
	if !ok {
		t.Fatal("a newly created leaf carries no boxwrightPlacement field, so it would never be a candidate")
	}
	if got.Type != homebox.FieldTypeText || got.TextValue != "true" {
		t.Errorf("boxwrightPlacement = %+v, want the text value \"true\" -- the fields= "+
			"query the box index runs matches text fields only", got)
	}
	if _, ok := find(leaf.Metadata(8, "normal", false)); ok {
		t.Error("a re-run re-asserted boxwrightPlacement, which would undo turning the location off")
	}
}

// But an explicit setting in the outline is an instruction, and still applies.
func TestExplicitOutlineSettingsApplyOnRerun(t *testing.T) {
	leaf := parse(t, "Garage\n  Fresno access=deep capacity=4\n")[0].Children[0]

	got := map[string]homebox.CustomField{}
	for _, f := range leaf.Metadata(8, "normal", false) {
		got[f.Name] = f
	}
	if got["access"].TextValue != "deep" {
		t.Errorf("access = %q, want deep", got["access"].TextValue)
	}
	if got["capacityUnits"].NumberValue != 4 {
		t.Errorf("capacityUnits = %v, want 4", got["capacityUnits"].NumberValue)
	}
}
