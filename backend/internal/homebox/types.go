package homebox

// Types target sysadminsmedia/homebox v0.26.x after the entity merge: items
// and locations are both "entities", locations flagged by their entity type's
// isLocation.
//
// Verified against a live v0.26.2 instance on 2026-09-08; captured response
// shapes live in testdata/. See docs/HOMEBOX.md for the evidence.

// Entity is a Homebox item or location (the subset of fields we use).
//
// Note the asymmetry around the parent: writes send a flat parentId (see
// CreateEntityRequest), but reads never return one -- responses carry a nested
// parent object instead. Decoding parentId here would silently yield "".
type Entity struct {
	ID          string      `json:"id,omitempty"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Quantity    int         `json:"quantity,omitempty"`
	Parent      *EntityRef  `json:"parent,omitempty"`
	EntityType  *EntityType `json:"entityType,omitempty"`
	// ItemCount is present on locations, and only when non-zero.
	ItemCount int   `json:"itemCount,omitempty"`
	Tags      []Tag `json:"tags,omitempty"`
	// Fields carries custom fields. It is ONLY populated by the detail
	// endpoint GET /v1/entities/{id} -- list rows never include it (verified:
	// 0/9 locations and 0/132 items in the live capture), which is why the
	// box index has to fetch each candidate location individually.
	Fields []CustomField `json:"fields,omitempty"`
}

// ParentID returns the parent's id, or "" at the root of the hierarchy.
func (e Entity) ParentID() string {
	if e.Parent == nil {
		return ""
	}
	return e.Parent.ID
}

// ParentName returns the parent's name, or "" at the root of the hierarchy.
func (e Entity) ParentName() string {
	if e.Parent == nil {
		return ""
	}
	return e.Parent.Name
}

// IsLocation reports whether this entity's type is a location type. Entities
// whose type was not expanded in the response are treated as items.
func (e Entity) IsLocation() bool {
	return e.EntityType != nil && e.EntityType.IsLocation
}

// Tag is a Homebox tag. Tags are returned FULLY EXPANDED on list rows, unlike
// custom fields, which makes them the only per-item attribute the box index
// can read without one request per item.
type Tag struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TagCreateRequest is the body for POST /v1/tags.
type TagCreateRequest struct {
	Name string `json:"name"`
}

// EntityRef is the abbreviated entity embedded as a parent.
type EntityRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Custom-field type discriminators (entityfield.Type in Homebox).
const (
	FieldTypeText    = "text"
	FieldTypeNumber  = "number"
	FieldTypeBoolean = "boolean"
	FieldTypeTime    = "time"
)

// CustomField is a Homebox entity custom field.
//
// Values are TYPED: the server always returns all three value keys and the
// caller must read the one named by Type. Reading textValue alone -- which an
// earlier version of this struct did -- silently yields 0 for every number and
// false for every boolean. That is not hypothetical: capacityUnits and
// fragileSafe are exactly such fields, and both hard exclusions in the
// placement engine key off them.
type CustomField struct {
	ID           string  `json:"id,omitempty"`
	Name         string  `json:"name"`
	Type         string  `json:"type,omitempty"`
	TextValue    string  `json:"textValue"`
	NumberValue  float64 `json:"numberValue"`
	BooleanValue bool    `json:"booleanValue"`
}

// Text returns the field's text value and whether it is a text field.
func (f CustomField) Text() (string, bool) {
	if f.Type != FieldTypeText {
		return "", false
	}
	return f.TextValue, true
}

// Number returns the field's numeric value and whether it is a number field.
func (f CustomField) Number() (float64, bool) {
	if f.Type != FieldTypeNumber {
		return 0, false
	}
	return f.NumberValue, true
}

// Bool returns the field's boolean value and whether it is a boolean field.
func (f CustomField) Bool() (bool, bool) {
	if f.Type != FieldTypeBoolean {
		return false, false
	}
	return f.BooleanValue, true
}

// FieldSet indexes custom fields by name. The bool result of each accessor
// distinguishes "absent, or present with the wrong type" from "present and
// zero", which the placement engine depends on: an unset fragileSafe means
// unknown, not false, and must not trigger a hard exclusion.
type FieldSet map[string]CustomField

// FieldSet indexes an entity's custom fields by name.
func (e Entity) FieldSet() FieldSet {
	fs := make(FieldSet, len(e.Fields))
	for _, f := range e.Fields {
		fs[f.Name] = f
	}
	return fs
}

// Text looks up a text field by name.
func (fs FieldSet) Text(name string) (string, bool) {
	f, ok := fs[name]
	if !ok {
		return "", false
	}
	return f.Text()
}

// Number looks up a number field by name.
func (fs FieldSet) Number(name string) (float64, bool) {
	f, ok := fs[name]
	if !ok {
		return 0, false
	}
	return f.Number()
}

// Bool looks up a boolean field by name.
func (fs FieldSet) Bool(name string) (bool, bool) {
	f, ok := fs[name]
	if !ok {
		return false, false
	}
	return f.Bool()
}

// CreateEntityRequest creates an item (or location) under a parent.
//
// EntityTypeID is optional for items -- the server auto-resolves the group's
// default type. But that default has isLocation:false, so creating a LOCATION
// requires passing a location type's id explicitly or you silently get an item.
// There is no fields key: custom fields are writable only via PUT.
type CreateEntityRequest struct {
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Quantity     int      `json:"quantity,omitempty"`
	ParentID     string   `json:"parentId,omitempty"`
	EntityTypeID string   `json:"entityTypeId,omitempty"`
	TagIDs       []string `json:"tagIds,omitempty"`
}

// EntityType mirrors /v1/entity-types entries.
type EntityType struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsLocation bool   `json:"isLocation"`
}

// LoginRequest is the body for POST /v1/users/login.
type LoginRequest struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	StayLoggedIn bool   `json:"stayLoggedIn"`
}

// LoginResponse is the reply from POST /v1/users/login.
type LoginResponse struct {
	Token           string `json:"token"`
	AttachmentToken string `json:"attachmentToken"`
}

// paginated wraps Homebox list responses under /v1/entities. Total is the full
// count across pages, not the length of Items.
type paginated struct {
	Items    []Entity `json:"items"`
	Page     int      `json:"page"`
	PageSize int      `json:"pageSize"`
	Total    int      `json:"total"`
}
