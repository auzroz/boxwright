// Package homebox is a minimal client for the sysadminsmedia/homebox
// v0.26.x API (unified /v1/entities). Stdlib only.
package homebox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Client talks to a single Homebox instance.
type Client struct {
	baseURL string // includes /api, no trailing slash
	token   string // hb_... API key or session token; sent as Bearer
	http    *http.Client
}

// New returns a Client. baseURL must include the /api prefix
// (e.g. http://homebox:7745/api). token may be a static API key (hb_...)
// or a session token from Login.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// BaseURL returns the instance this client talks to, normalized. Exported so
// a caller holding several clients can tell them apart -- the API layer keys
// its per-Homebox caches on this and the token.
func (c *Client) BaseURL() string { return c.baseURL }

// Token returns the bearer credential. Exported for the same reason, and for
// that reason only: it is a secret, and nothing should log or return it.
func (c *Client) Token() string { return c.token }

// Login exchanges credentials for a session token and stores it on the
// client. Prefer static API keys in production; this exists for dev flows.
func (c *Client) Login(ctx context.Context, username, password string) error {
	var out LoginResponse
	err := c.do(ctx, http.MethodPost, "/v1/users/login", LoginRequest{
		Username: username, Password: password, StayLoggedIn: true,
	}, &out)
	if err != nil {
		return fmt.Errorf("homebox login: %w", err)
	}
	c.token = strings.TrimPrefix(out.Token, "Bearer ")
	return nil
}

// defaultPageSize bounds a single request. The live server applies no default
// page cap (verified: omitting pageSize returned all 132 rows with
// pageSize:-1), so this is a guard against a large inventory or a future
// server change, not a fix for observed truncation.
const defaultPageSize = 200

// ListEntities returns entities, optionally filtered to children of parentIDs
// and/or by isLocation. It pages until it has collected Total rows.
//
// isLocation is a *bool because absent and false are different queries:
// omitting the parameter returns items only, NOT everything.
func (c *Client) ListEntities(ctx context.Context, parentIDs []string, isLocation *bool) ([]Entity, error) {
	q := url.Values{}
	for _, id := range parentIDs {
		q.Add("parentIds", id)
	}
	if isLocation != nil {
		q.Set("isLocation", strconv.FormatBool(*isLocation))
	}
	return c.listPaged(ctx, q)
}

// ErrFieldFilter marks a custom-field filter this client refuses to send.
var ErrFieldFilter = errors.New("invalid custom-field filter")

// ListEntitiesByField returns the entities whose custom field name holds any
// of values. Repeated fields parameters are OR, so several values cost one
// query rather than one each.
//
// Measured against v0.26.2: the match is EXACT and CASE-SENSITIVE on both
// halves, and it works on TEXT fields ONLY -- a location really holding
// capacityUnits=8 is not returned by fields=capacityUnits=8, because that
// field is a number. Anything meant to be queryable has to be stored as text.
//
// The name and the value are joined HERE, and an "=" in either half is
// rejected, because `fields=Name` with no "=" is not an error: it is silently
// ignored and the response is the ENTIRE inventory. A caller that formats the
// pair itself can produce that shape from an empty variable and get "found
// everything" where it meant "found nothing" -- so the unsafe shape is made
// unrepresentable instead of guarded against at each call site.
func (c *Client) ListEntitiesByField(ctx context.Context, name string, values []string, isLocation *bool) ([]Entity, error) {
	if name == "" || strings.Contains(name, "=") {
		return nil, fmt.Errorf("%w: field name %q", ErrFieldFilter, name)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%w: no values for %q", ErrFieldFilter, name)
	}
	q := url.Values{}
	for _, v := range values {
		if v == "" || strings.Contains(v, "=") {
			return nil, fmt.Errorf("%w: value %q for %q", ErrFieldFilter, v, name)
		}
		q.Add("fields", name+"="+v)
	}
	if isLocation != nil {
		q.Set("isLocation", strconv.FormatBool(*isLocation))
	}
	return c.listPaged(ctx, q)
}

// listPaged runs one /v1/entities query to exhaustion.
func (c *Client) listPaged(ctx context.Context, q url.Values) ([]Entity, error) {
	out := []Entity{}
	for page := 1; ; page++ {
		q.Set("page", strconv.Itoa(page))
		q.Set("pageSize", strconv.Itoa(defaultPageSize))

		var got paginated
		if err := c.do(ctx, http.MethodGet, "/v1/entities?"+q.Encode(), nil, &got); err != nil {
			return nil, fmt.Errorf("homebox list entities (page %d): %w", page, err)
		}
		out = append(out, got.Items...)

		// Stop on a short page as well as on the count, so a server that
		// ignores paging entirely (returning everything on page 1) terminates
		// instead of looping forever on identical responses.
		if len(got.Items) == 0 || len(out) >= got.Total || len(got.Items) < defaultPageSize {
			return out, nil
		}
	}
}

// GetEntity fetches one entity by id. This is the ONLY endpoint that returns
// custom fields -- list rows never carry them -- so reading box metadata costs
// one call per location.
func (c *Client) GetEntity(ctx context.Context, id string) (Entity, error) {
	var out Entity
	if err := c.do(ctx, http.MethodGet, "/v1/entities/"+url.PathEscape(id), nil, &out); err != nil {
		return Entity{}, fmt.Errorf("homebox get entity %s: %w", id, err)
	}
	return out, nil
}

// readOnlyEntityKeys are keys the GET returns that the PUT does not accept, or
// that it expresses differently. They are stripped or translated before the
// write; everything else is passed through verbatim.
var readOnlyEntityKeys = []string{
	"createdAt", "updatedAt", "attachments", "children",
	"imageId", "thumbnailId", "totalPrice", "itemCount",
	"parent", "entityType", "tags",
}

// SetFields merges want into an entity's custom fields.
//
// POST cannot write fields and PUT is a FULL REPLACE, so this reads the entity
// fresh, changes only the fields array, and writes the whole object back.
//
// It deliberately round-trips the raw JSON object rather than a typed struct.
// A typed struct silently blanks every column it forgot to model, and this is
// not hypothetical -- an earlier typed version of this function was measured
// against a live entity and destroyed purchasePrice, purchaseFrom,
// serialNumber, notes, manufacturer, modelNumber and insured in a single call.
// Passing the object through means a field we do not model, including one a
// future Homebox version adds, survives by construction.
func (c *Client) SetFields(ctx context.Context, entityID string, want []CustomField) (Entity, error) {
	return c.UpdateFields(ctx, entityID, func(FieldSet) []CustomField { return want })
}

// UpdateFields is SetFields for a write that depends on what is there now:
// change is given the entity's current fields, as just read, and returns the
// ones to write -- adding to a fill level, or declining to overwrite a newer
// observation. One GET and one PUT, the same raw-JSON round trip as SetFields.
//
// When change returns nothing, nothing is written and the entity is returned
// as read: a write that has nothing to say must not bump updatedAt or echo
// down the change feed.
//
// Not atomic against another writer between the GET and the PUT -- Homebox
// has no conditional update -- so callers serialise their own writes to one
// entity; a concurrent edit in Homebox's UI in that window is lost, as it
// would be to SetFields.
func (c *Client) UpdateFields(ctx context.Context, entityID string, change func(FieldSet) []CustomField) (Entity, error) {
	path := "/v1/entities/" + url.PathEscape(entityID)

	var raw map[string]json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return Entity{}, fmt.Errorf("homebox set fields: read %s: %w", entityID, err)
	}

	var cur Entity
	if err := remarshal(raw, &cur); err != nil {
		return Entity{}, fmt.Errorf("homebox set fields: decode %s: %w", entityID, err)
	}
	want := change(cur.FieldSet())
	if len(want) == 0 {
		return cur, nil
	}

	// Translate the read-only shapes the PUT expects as flat ids.
	if id := cur.ParentID(); id != "" {
		raw["parentId"] = mustJSON(id)
	}
	if cur.EntityType != nil {
		raw["entityTypeId"] = mustJSON(cur.EntityType.ID)
	}
	if tagsRaw, ok := raw["tags"]; ok {
		var tags []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(tagsRaw, &tags); err == nil {
			ids := []string{}
			for _, t := range tags {
				ids = append(ids, t.ID)
			}
			raw["tagIds"] = mustJSON(ids)
		}
	}
	for _, k := range readOnlyEntityKeys {
		delete(raw, k)
	}

	// Merge by field name, keeping each existing field's server-side id.
	merged := make([]CustomField, len(cur.Fields))
	copy(merged, cur.Fields)
	for _, w := range want {
		replaced := false
		for i := range merged {
			if merged[i].Name == w.Name {
				w.ID = merged[i].ID
				merged[i] = w
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, w)
		}
	}
	raw["fields"] = mustJSON(merged)
	// Explicit, never defaulted: a metadata write must not relocate children.
	raw["syncChildEntityLocations"] = mustJSON(false)

	var out Entity
	if err := c.do(ctx, http.MethodPut, path, raw, &out); err != nil {
		return Entity{}, fmt.Errorf("homebox set fields on %s: %w", entityID, err)
	}
	return out, nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Only reachable with a type json/encoding cannot represent; every
		// call site passes strings, slices of string, bools or CustomField.
		panic("homebox: marshalling a value we control: " + err.Error())
	}
	return b
}

func remarshal(raw map[string]json.RawMessage, out any) error {
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// ListTags returns every tag in the group. Bare array, like entity types.
func (c *Client) ListTags(ctx context.Context) ([]Tag, error) {
	out := []Tag{}
	if err := c.do(ctx, http.MethodGet, "/v1/tags", nil, &out); err != nil {
		return nil, fmt.Errorf("homebox list tags: %w", err)
	}
	return out, nil
}

// ResolveTag finds a tag matching name, creating it only if nothing does.
//
// fold reduces a tag name to the identity the caller matches on; nil means
// case-insensitive comparison. It is a parameter rather than a constant here
// because the identity that matters belongs to the caller's domain: the API
// layer addresses categories by their normalized key, so "Micro Masterpieces"
// and "micro-masterpieces" have to resolve to the same tag. Comparing case
// only would miss that, create a second tag, and detach every newly filed item
// from the tag the user already had -- the exact failure this signature exists
// to prevent. Duplicating placement's fold inside this package instead would
// let the two drift silently.
func (c *Client) ResolveTag(ctx context.Context, name string, fold func(string) string) (Tag, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Tag{}, fmt.Errorf("homebox resolve tag: empty name")
	}
	if fold == nil {
		fold = strings.ToLower
	}
	tags, err := c.ListTags(ctx)
	if err != nil {
		return Tag{}, err
	}
	want := fold(name)
	for _, t := range tags {
		if fold(t.Name) == want {
			return t, nil
		}
	}
	var out Tag
	if err := c.do(ctx, http.MethodPost, "/v1/tags", TagCreateRequest{Name: titleCase(name)}, &out); err != nil {
		return Tag{}, fmt.Errorf("homebox create tag %q: %w", name, err)
	}
	return out, nil
}

// titleCase renders "seasonal-holiday" as "Seasonal Holiday".
//
// Decodes the first RUNE rather than slicing the first byte: w[:1] splits a
// multi-byte character in half, so a tag like "Éclairage" would be created with
// invalid UTF-8 in its name, which json.Marshal then replaces with U+FFFD --
// a permanently mis-named tag only the user can fix, by hand.
func titleCase(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	for i, w := range words {
		if w == "" {
			continue
		}
		r, n := utf8.DecodeRuneInString(w)
		words[i] = string(unicode.ToUpper(r)) + strings.ToLower(w[n:])
	}
	return strings.Join(words, " ")
}

// CreateEntity creates an item or location. Returns the created entity.
//
// To create a LOCATION, req.EntityTypeID must name a type whose isLocation is
// true (see LocationEntityTypeID). With it omitted the server auto-resolves the
// group's default type, which is a non-location type, and the caller silently
// gets an item.
func (c *Client) CreateEntity(ctx context.Context, req CreateEntityRequest) (Entity, error) {
	var out Entity
	if err := c.do(ctx, http.MethodPost, "/v1/entities", req, &out); err != nil {
		return Entity{}, fmt.Errorf("homebox create entity: %w", err)
	}
	return out, nil
}

// ListEntityTypes returns the configured entity types. Unlike /v1/entities
// this endpoint returns a bare array, not a paginated wrapper.
func (c *Client) ListEntityTypes(ctx context.Context) ([]EntityType, error) {
	out := []EntityType{}
	if err := c.do(ctx, http.MethodGet, "/v1/entity-types", nil, &out); err != nil {
		return nil, fmt.Errorf("homebox list entity types: %w", err)
	}
	return out, nil
}

// LocationEntityTypeID returns the id of an entity type whose isLocation is
// true. Creating a location REQUIRES one: with entityTypeId omitted the server
// auto-resolves the group's default type, which is a non-location type, and
// the "new box" is silently created as an item.
func (c *Client) LocationEntityTypeID(ctx context.Context) (string, error) {
	types, err := c.ListEntityTypes(ctx)
	if err != nil {
		return "", err
	}
	for _, t := range types {
		if t.IsLocation {
			return t.ID, nil
		}
	}
	return "", fmt.Errorf("homebox: no entity type with isLocation=true (found %d types)", len(types))
}

// UploadAttachment attaches a photo to an entity.
//
// Multipart contract verified against v0.26.2: "file" and "name" are both
// REQUIRED (the server rejects a missing name), and "type" is optional -- when
// empty the server infers "photo" from an image mime type, which is better
// than asserting an enum value we would have to keep in sync.
func (c *Client) UploadAttachment(ctx context.Context, entityID, filename string, data []byte, primary bool) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	_ = w.WriteField("name", filename)
	_ = w.WriteField("primary", fmt.Sprintf("%t", primary))
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/entities/"+url.PathEscape(entityID)+"/attachments", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("homebox upload attachment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("homebox upload attachment: status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// do executes a JSON request and decodes the response into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, string(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
