package bootstrap

import (
	"context"
	"fmt"
	"strings"

	"boxwright/internal/homebox"
)

// Action is one planned change, so a dry run can print exactly what an apply
// would do.
type Action struct {
	Path     string // "Garage > California > Fresno"
	Create   bool   // false means it already exists and is only being annotated
	SetMeta  bool
	ID       string // populated after Apply, or when the location already exists
	Metadata []homebox.CustomField
}

func (a Action) String() string {
	switch {
	case a.Create && a.SetMeta:
		return "create + annotate  " + a.Path
	case a.Create:
		return "create             " + a.Path
	case a.SetMeta:
		return "annotate           " + a.Path
	default:
		return "unchanged          " + a.Path
	}
}

// Client is the subset of the Homebox client bootstrap needs, so the planner
// can be tested without a server.
type Client interface {
	ListEntities(ctx context.Context, parentIDs []string, isLocation *bool) ([]homebox.Entity, error)
	CreateEntity(ctx context.Context, req homebox.CreateEntityRequest) (homebox.Entity, error)
	SetFields(ctx context.Context, entityID string, want []homebox.CustomField) (homebox.Entity, error)
	LocationEntityTypeID(ctx context.Context) (string, error)
}

// Metadata renders a leaf's settings as Homebox custom fields.
//
// Two rules, both about not asserting things we do not know:
//
//   - Unset heavySafe/fragileSafe are omitted, never defaulted to false.
//     Absent means "unknown" to the placement engine, which keeps the box in
//     the running with a caveat; false would exclude it from every fragile or
//     heavy item forever.
//   - Defaults for capacity and access apply only when CREATING the location.
//     Homebox is the system of record, so on a re-run an outline that is
//     silent about a setting must leave whatever the user last chose alone --
//     otherwise editing access to "deep" in the Homebox UI is quietly undone
//     the next time the outline is applied. Explicit outline settings still
//     win, because writing them down is how you say you mean them.
func (n *Node) Metadata(defaultCapacity int, defaultAccess string, creating bool) []homebox.CustomField {
	if !n.Leaf() {
		// A node with children under it is a grouping in the outline, not a
		// container the outline is declaring. The user can still opt it in
		// afterwards from the location picker -- nothing here excludes it,
		// this only decides what the outline itself asserts.
		return nil
	}
	out := []homebox.CustomField{}
	// Writing the outline down IS the user saying these are places to put
	// things, so a leaf being CREATED is opted in to automated placement. On a
	// re-run it is not re-asserted, for the same reason the defaults below are
	// not: turning a location off in the picker is a decision, and an outline
	// that is silent about it must not quietly undo it.
	//
	// The name must match fieldPlacement/placementYes in
	// internal/api/locations.go; TestBootstrapMarksNewLeavesEligible asserts
	// the two packages still agree.
	if creating {
		out = append(out, homebox.CustomField{
			Name: "boxwrightPlacement", Type: homebox.FieldTypeText, TextValue: "true",
		})
	}

	capacity := 0
	if creating {
		capacity = defaultCapacity
	}
	if n.CapacityUnits != nil {
		capacity = *n.CapacityUnits
	}
	if capacity > 0 {
		out = append(out, homebox.CustomField{
			Name: "capacityUnits", Type: homebox.FieldTypeNumber, NumberValue: float64(capacity),
		})
	}
	access := ""
	if creating {
		access = defaultAccess
	}
	if n.Access != "" {
		access = n.Access
	}
	if access != "" {
		out = append(out, homebox.CustomField{
			Name: "access", Type: homebox.FieldTypeText, TextValue: access,
		})
	}
	if n.HeavySafe != nil {
		out = append(out, homebox.CustomField{
			Name: "heavySafe", Type: homebox.FieldTypeBoolean, BooleanValue: *n.HeavySafe,
		})
	}
	if n.FragileSafe != nil {
		out = append(out, homebox.CustomField{
			Name: "fragileSafe", Type: homebox.FieldTypeBoolean, BooleanValue: *n.FragileSafe,
		})
	}
	return out
}

// Options tune what the outline does not say.
type Options struct {
	DefaultCapacity int    // applied to leaves with no capacity= setting
	DefaultAccess   string // applied to leaves with no access= setting
	Apply           bool   // false plans without writing anything
}

// Run plans the outline against Homebox and, when opts.Apply is set, creates
// the missing locations and writes leaf metadata.
//
// Idempotent: a location that already exists under the same parent, matched by
// name, is reused rather than duplicated, so the outline can be re-run after
// editing. Matching is case-insensitive because "california" and "California"
// are the same row to a person.
func Run(ctx context.Context, c Client, roots []*Node, opts Options) ([]Action, error) {
	isLoc := true
	existing, err := c.ListEntities(ctx, nil, &isLoc)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: read existing locations: %w", err)
	}

	// key is parentID + "/" + lowercased name.
	byKey := map[string]homebox.Entity{}
	for _, e := range existing {
		byKey[childKey(e.ParentID(), e.Name)] = e
	}

	var locTypeID string
	if opts.Apply {
		if locTypeID, err = c.LocationEntityTypeID(ctx); err != nil {
			// Without a location entity type, CreateEntity silently makes
			// items instead of locations. Refuse rather than corrupt.
			return nil, fmt.Errorf("bootstrap: %w", err)
		}
	}

	actions := []Action{}
	// parentReal distinguishes "this node hangs off the root" from "this node
	// hangs off a parent that does not exist yet". Without it, a dry run would
	// look a child up under an empty parent id and could match an unrelated
	// TOP-LEVEL location of the same name -- reporting "unchanged" for a box
	// it has never seen. Names like "Fresno" are exactly the kind that recur.
	var visit func(nodes []*Node, parentID string, parentReal bool, prefix []string) error
	visit = func(nodes []*Node, parentID string, parentReal bool, prefix []string) error {
		for _, n := range nodes {
			path := append(append([]string{}, prefix...), n.Name)
			act := Action{Path: strings.Join(path, " > ")}

			id := ""
			if found, ok := byKey[childKey(parentID, n.Name)]; parentReal && ok {
				id = found.ID
			} else {
				act.Create = true
			}
			act.Metadata = n.Metadata(opts.DefaultCapacity, opts.DefaultAccess, act.Create)
			act.SetMeta = len(act.Metadata) > 0

			if opts.Apply {
				if act.Create {
					created, err := c.CreateEntity(ctx, homebox.CreateEntityRequest{
						Name:         n.Name,
						ParentID:     parentID,
						EntityTypeID: locTypeID, // required, or this becomes an item
					})
					if err != nil {
						return fmt.Errorf("create %q: %w", act.Path, err)
					}
					id = created.ID
				}
				if act.SetMeta {
					if _, err := c.SetFields(ctx, id, act.Metadata); err != nil {
						return fmt.Errorf("annotate %q: %w", act.Path, err)
					}
				}
			}

			act.ID = id
			actions = append(actions, act)
			// In a dry run a created node has no id, so its children are
			// likewise unresolvable and must all read as creates.
			if err := visit(n.Children, id, id != "", path); err != nil {
				return err
			}
		}
		return nil
	}

	if err := visit(roots, "", true, nil); err != nil {
		return actions, err
	}
	return actions, nil
}

func childKey(parentID, name string) string {
	return parentID + "/" + strings.ToLower(strings.TrimSpace(name))
}
