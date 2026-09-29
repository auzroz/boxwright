// Package placement contains the rules-based box recommendation engine.
// This is the heart of the project. It must work with zero AI calls; an
// optional LLM Reranker can refine the rules-based shortlist later.
//
// Heuristics encoded (professional organizer / self-storage practice):
// like-with-like category grouping, capacity fit, fragile items never into a
// box recorded as crush-risk, heavy items never into one recorded as not
// heavy-safe, frequently accessed categories towards an easy-to-reach
// location, and things too big for a container into the area that holds the
// containers.
//
// There is deliberately NO spatial model. "Zones", floor levels and
// front-of-unit ordering are NOT implemented -- the only positional signal is
// the Access bucket (easy/normal/deep), and GridX/GridY are stored but unused.
// This comment claimed otherwise for months and the claim propagated into
// three documents; if you add one, change this comment in the same commit.
package placement

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ItemDraft is an identified (or manually entered) item awaiting placement.
// JSON tags are the API contract shared with app/src/types.ts.
type ItemDraft struct {
	Name        string  `json:"name"`
	Category    string  `json:"category"`
	SizeBucket  string  `json:"sizeBucket"` // S, M, L, XL -- see Bulky
	Fragile     bool    `json:"fragile"`
	WeightClass string  `json:"weightClass"` // light, medium, heavy
	Notes       string  `json:"notes"`
	Confidence  float64 `json:"confidence"` // 0 for manual entry
	// Quantity is how many identical things this capture represents. Filling a
	// storage unit means twelve mason jars and six sets of sheets; without
	// this each one costs its own photo, vision call and round trip.
	Quantity int `json:"quantity"` // Normalize() floors this at 1
	// Region is where in the photo this item is, when the model said. The app
	// crops the shared photo to it so each item is reviewed against a picture
	// of ITSELF rather than of the whole shelf -- which is the difference
	// between confirming an item and guessing which of eight things a row is
	// about.
	//
	// A POINTER, and nil is the normal case. Plenty of models will not produce
	// a usable box, and a wrong one is worse than none: it crops away the
	// object the user is being asked to identify. Everything downstream falls
	// back to the whole photo, which is exactly what it showed before this
	// existed.
	Region *Region `json:"region,omitempty"`
	// Bulky means this does not go inside a container at all: a lawn mower, a
	// bicycle, a floor lamp, a dining chair.
	//
	// A separate fact from SizeBucket rather than a bucket above XL, because it
	// is a difference of KIND, not of degree. The buckets measure an item
	// against a container -- S is a shoebox, XL fills a large tote -- and a
	// mower is not a bigger tote-load, it is something you stand on a floor.
	// Reported by a real user: XL is 8 units and an empty default container is
	// 8 units, so a lawn mower scored as exactly filling one and was
	// confidently recommended into it.
	Bulky bool `json:"bulky"`
	// DimensionsCm is the item's size, when anything better than the bucket is
	// known, and DimensionsSource says what: "lidar" or "manual" are
	// measurements and may exclude a container the item will not go into;
	// "vision" is the model's estimate from what the object is, in a photo
	// with no scale in it, and only ever informs how much room it takes.
	DimensionsCm     *Dims  `json:"dimensionsCm,omitempty"`
	DimensionsSource string `json:"dimensionsSource,omitempty"`
}

// Region is a rectangle within the photo, as FRACTIONS of its width and
// height (0..1) with the origin top-left.
//
// Fractions, never pixels. The photo is downscaled on the way in and again by
// whatever the model was shown, so a pixel box would be measured against an
// image nobody else has. Fractions survive every resize.
type Region struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// UnmarshalJSON decodes a region and NEVER fails.
//
// This is the whole reason it exists. Region is a typed field, so before this
// a model that answered `"region": [0.1, 0.2, 0.3, 0.4]` -- the shape most
// detection training data uses -- made json.Unmarshal reject the entire
// array, and unmarshalDrafts turns that into "model returned unparseable
// drafts". One badly shaped optional box would have discarded every item in
// the photo and dropped the user into manual entry for all of them. An
// unrecognised key used to be ignored; adding a typed field must not make the
// reply more brittle than it was before the field existed.
//
// A four-number array is deliberately NOT interpreted. [x,y,w,h] and
// [x0,y0,x1,y1] are both common and indistinguishable, so guessing produces a
// box around the wrong thing half the time -- and a wrong box is worse than
// none, because it crops away the object the user is being asked to identify.
// Anything that is not the documented object shape becomes no region at all,
// and the card shows the whole photo.
func (r *Region) UnmarshalJSON(b []byte) error {
	// A shadow type, so decoding into it does not re-enter this method.
	type plain Region
	var obj plain
	if err := json.Unmarshal(b, &obj); err == nil {
		*r = Region(obj)
		return nil
	}
	*r = Region{}
	return nil
}

const (
	// minRegionSide is the smallest edge worth cropping to. Below this the
	// crop is a handful of pixels of texture -- less identifiable than the
	// whole photo, which is the thing it would be replacing.
	minRegionSide = 0.02

	// maxRegionArea is the largest a box may be and still be a crop.
	//
	// Cropping a photo to a third of itself shows the user the shelf again,
	// not the item, so it cannot help them tell one card from another -- and
	// it costs a decode and a file to say nothing. Measured for scale: on a
	// cluttered desk photo Claude's boxes were 0.4% to 7% of the frame, while
	// a model that was guessing produced boxes of 42% and 56%.
	maxRegionArea = 0.35

	// fabricationGrid is the tell that a model is inventing boxes instead of
	// looking.
	//
	// Measured, on the photo above: gemma3:4b answered 7 items whose 28
	// coordinates were EVERY ONE a multiple of 0.05 -- 0.20, 0.30, 0.60,
	// 0.15 -- and every box was wrong. Claude, on the same photo, put 7 of 28
	// on that grid and every box was right. Real localisation does not land on
	// a round number twenty-eight times in a row.
	fabricationGrid = 0.05
)

// normalized returns the region clamped inside the image, and whether what is
// left is worth using.
//
// Models produce boxes that run off the edge, boxes with negative width, and
// occasionally coordinates in pixels because the prompt was ignored. Clamping
// handles the first two. The third is indistinguishable from an absurdly large
// fraction: the origin clamps to the far corner, nothing is left to the right
// of it, and the box is dropped -- so the user sees the photo they took, which
// is the safe direction.
func (r Region) normalized() (Region, bool) {
	if math.IsNaN(r.X) || math.IsNaN(r.Y) || math.IsNaN(r.W) || math.IsNaN(r.H) {
		return Region{}, false
	}
	if r.W <= 0 || r.H <= 0 {
		return Region{}, false
	}
	x := math.Max(0, math.Min(1, r.X))
	y := math.Max(0, math.Min(1, r.Y))
	w := math.Min(r.W, 1-x)
	h := math.Min(r.H, 1-y)
	if w < minRegionSide || h < minRegionSide {
		return Region{}, false
	}
	if w*h > maxRegionArea {
		return Region{}, false
	}
	return Region{X: x, Y: y, W: w, H: h}, true
}

// onFabricationGrid reports whether every coordinate is a round multiple of
// fabricationGrid.
func (r Region) onFabricationGrid() bool {
	for _, v := range [4]float64{r.X, r.Y, r.W, r.H} {
		if math.Abs(math.Round(v/fabricationGrid)*fabricationGrid-v) > 1e-9 {
			return false
		}
	}
	return true
}

// DropFabricatedRegions removes every region from a set of drafts when the set
// looks invented rather than observed, and reports how many it dropped.
//
// Whether a model can point at a thing is a capability, not a setting, and
// there is no way to ask. What CAN be seen is the shape of the answer: a model
// that is guessing writes round numbers. Across several items that is decisive
// -- twenty-eight coordinates all landing on a twentieth is not something
// measurement does.
//
// All or nothing on purpose. The tell is a property of the answer as a whole,
// and a model that fabricated six boxes did not carefully measure the seventh.
//
// The cost of being wrong here is that a good model's coarse boxes are ignored
// and the user sees the whole photo -- exactly what they saw before any of this
// existed. The cost of the opposite mistake is a confident picture of the wrong
// object beside the name of a different one, which is worse than no picture,
// and was the first thing a real user noticed.
func DropFabricatedRegions(drafts []ItemDraft) int {
	withRegion := 0
	for i := range drafts {
		if drafts[i].Region != nil {
			withRegion++
		}
	}
	// One box has too few digits to judge; let it through and let the area and
	// size rules be the only guard.
	if withRegion < 2 {
		return 0
	}
	for i := range drafts {
		if drafts[i].Region != nil && !drafts[i].Region.onFabricationGrid() {
			return 0
		}
	}
	dropped := 0
	for i := range drafts {
		if drafts[i].Region != nil {
			drafts[i].Region = nil
			dropped++
		}
	}
	return dropped
}

// Normalize clamps free-text model output into the expected vocabularies.
func (d *ItemDraft) Normalize() {
	d.SizeBucket = strings.ToUpper(strings.TrimSpace(d.SizeBucket))
	if _, ok := NominalLitres[d.SizeBucket]; !ok {
		d.SizeBucket = "M"
	}
	d.WeightClass = strings.ToLower(strings.TrimSpace(d.WeightClass))
	switch d.WeightClass {
	case "light", "medium", "heavy":
	default:
		d.WeightClass = "medium"
	}
	d.Category = NormalizeCategory(d.Category)
	if d.Confidence < 0 {
		d.Confidence = 0
	}
	if d.Confidence > 1 {
		d.Confidence = 1
	}
	if d.Quantity < 1 {
		d.Quantity = 1
	}
	// A region that does not survive clamping is dropped rather than repaired.
	// The fallback -- show the whole photo -- is always correct, so there is
	// never a reason to hand the app a box we do not believe.
	if d.Region != nil {
		if fixed, ok := d.Region.normalized(); ok {
			d.Region = &fixed
		} else {
			d.Region = nil
		}
	}
	// The same for a size: an unbelievable one is dropped and the bucket
	// stands in. A size with no recognised source is treated as an estimate,
	// which can never exclude -- the safe reading of a claim nobody vouched for.
	if d.DimensionsCm != nil {
		if fixed, ok := d.DimensionsCm.Normalize(); ok {
			d.DimensionsCm = &fixed
		} else {
			d.DimensionsCm = nil
		}
	}
	switch {
	case d.DimensionsCm == nil:
		d.DimensionsSource = ""
	case d.DimensionsSource != DimsLidar && d.DimensionsSource != DimsManual:
		d.DimensionsSource = DimsVision
	}
}

// Box is a placement candidate, built from a Homebox location the user has
// opted in to automated placement, plus its custom fields and cached contents.
//
// The engine has no opinion about what shape a container is. A box, a shelf, a
// cupboard, a whole room -- if the user said things may go there, it is a
// candidate and is scored like any other.
type Box struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ParentID and Area describe where the box lives. Area is the parent
	// location's name -- whatever the user calls the level above this one --
	// derived from the hierarchy rather than stored, so there is only ever one
	// source of truth for it.
	ParentID string `json:"parentId"`
	Area     string `json:"area"`
	// Access is how much effort it takes to reach this box:
	// "easy", "normal", "deep", or "" when unrecorded.
	Access string `json:"access"`
	// HeavySafe and FragileSafe are TRI-STATE on purpose. nil means "nobody
	// has recorded this", which is the normal state of a fresh inventory, and
	// must not be read as false: a hard exclusion on absent metadata makes
	// cold start self-reinforcing, since every new container we then suggest
	// is itself created without metadata.
	HeavySafe   *bool `json:"heavySafe"`
	FragileSafe *bool `json:"fragileSafe"`
	// ContainerType is the user's own name for what kind of container this is
	// ("27-gallon", "shoebox"). Nothing is shipped; the list of types is
	// derived from the containers that carry one (ContainerTypes).
	ContainerType string `json:"containerType"`
	// CapacityL is how much it holds, in litres, and nil when nobody recorded
	// it. nil is UNKNOWN, exactly like a nil HeavySafe, and never excludes.
	CapacityL *int `json:"capacityL"`
	// InteriorCm is the inside size, when recorded. With a MEASURED item size
	// it decides whether the item goes in at all, which capacity cannot: a
	// 90 cm tripod does not go in a 60 cm container however empty it is.
	InteriorCm *Dims `json:"interiorCm"`
	// FillPct is how full it is, 0-100, meaningful only when FillSource says
	// where it came from (see fillKnown). An estimate may run past 100.
	FillPct *float64 `json:"fillPct"`
	// FillSource is "observed", "lidar", "estimated", or "" for unknown. Only
	// an observation is a fact that may exclude; an estimate -- Boxwright
	// adding up what it filed -- may only lower a score.
	FillSource string `json:"fillSource"`
	// FillCheckedAt is when the fill was last OBSERVED (RFC3339). Estimates
	// added since do not move it, so it says how stale the fact underneath is.
	FillCheckedAt string `json:"fillCheckedAt"`
	// CapacityUnits and UsedUnits are for clients that predate litres, derived
	// by SetLegacyUnits from what is known and 0/0 when it is not. The engine
	// never reads them. They used to be its whole model: 8 units assumed, every
	// item charged 2, so an unannotated container read full at four items and
	// vanished from the candidates.
	CapacityUnits int `json:"capacityUnits"`
	UsedUnits     int `json:"usedUnits"`
	// ItemCount is how many things Homebox lists inside, counting quantity.
	// Unlike UsedUnits it is a fact read from Homebox, not an estimate, so it
	// is what says whether a box is empty.
	ItemCount  int            `json:"itemCount"`
	Categories map[string]int `json:"categories"` // category -> item count inside
	GridX      int            `json:"gridX"`
	GridY      int            `json:"gridY"`
	// IsArea marks a location that holds OTHER locations -- a garage, a
	// storage unit, a room. Somewhere you stand a lawn mower, rather than
	// somewhere you put a box.
	//
	// Derived from the hierarchy, not from a setting, and used for exactly one
	// question: where does a thing go when it does not go in a container. It
	// is NOT a judgement about whether the location can hold items, which is
	// the user's to make and is recorded per location as boxwrightPlacement.
	IsArea bool `json:"isArea"`
	// AreaDepth is how far an area sits from the top of the hierarchy: 0 for a
	// garage or a storage unit, 1 for a row inside one.
	//
	// It exists because the outer levels are the real physical spaces. Asked
	// where a lawn mower goes, a user answered "the garage or storage unit 1"
	// -- not the row of totes inside the garage, which is a grouping rather
	// than somewhere with floor. Every area otherwise scores identically, so
	// without this the list came back alphabetically and offered "Arizona"
	// ahead of "Garage".
	AreaDepth int `json:"areaDepth,omitempty"`
}

// isFalse reports whether a tri-state flag was explicitly recorded as false.
// An unrecorded flag is unknown, never a reason to exclude.
func isFalse(b *bool) bool { return b != nil && !*b }

// isTrue reports whether a tri-state flag was explicitly recorded as true.
func isTrue(b *bool) bool { return b != nil && *b }

// Candidate is a scored box with human-readable reasoning.
type Candidate struct {
	Box     Box      `json:"box"`
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons"`
}

// NewContainerSuggestion is the fallback when no existing box fits.
type NewContainerSuggestion struct {
	SizeBucket string `json:"sizeBucket"` // suggested container size class
	Access     string `json:"access"`
	Label      string `json:"label"` // suggested box label
	Reason     string `json:"reason"`
	// ContainerType, CapacityL and InteriorCm name one of the user's own
	// container types when one suits the item, so the new container is
	// created already knowing its size. Empty when they have recorded none,
	// and the suggestion falls back to the size bucket.
	ContainerType string `json:"containerType,omitempty"`
	CapacityL     int    `json:"capacityL,omitempty"`
	InteriorCm    *Dims  `json:"interiorCm,omitempty"`
}

// NoPlaceForItem is the answer for something that fits nowhere AND cannot be
// solved by a new container -- which today means a bulky item with no area to
// stand it in. Distinct from NewContainer because the two ask the user for
// completely different things.
type NoPlaceForItem struct {
	Reason string `json:"reason"`
}

// Recommendation is the engine output.
//
// At most one of NewContainer and NoPlace is ever set: they are the two shapes
// of "nothing here fits", and which one applies depends on whether the item
// goes in a container at all.
type Recommendation struct {
	Candidates   []Candidate             `json:"candidates"`
	NewContainer *NewContainerSuggestion `json:"newContainer,omitempty"`
	NoPlace      *NoPlaceForItem         `json:"noPlace,omitempty"`
}

// Reranker optionally reorders the rules-based shortlist (e.g. an LLM given
// compact box manifests). Not implemented yet; see CLAUDE.md roadmap.
type Reranker interface {
	Rerank(ctx context.Context, item ItemDraft, shortlist []Candidate) ([]Candidate, error)
}

// Weights centralizes all scoring knobs. Every change needs a test.
//
// One invariant binds these together and is enforced by
// TestConvenienceAloneCannotClearTheBar: CapacityHeadway + AccessBonus must be
// strictly less than MinViableScore. Like-with-like is the whole premise of
// the engine, and it is enforced HERE, by the threshold -- not by a hard
// filter. If convenience alone can clear the bar, an empty easy-to-reach box
// beats the new-container suggestion for an item it has nothing in common
// with, and the recommendation is worse than useless because it is confident.
type Weights struct {
	CategoryMatch   float64 // scaled by the matching share of the box's contents
	CapacityHeadway float64 // bonus for comfortable remaining space
	// NeutralHeadroom stands in for CapacityHeadway when a box's capacity or
	// fill is not recorded: scored as though half full, below a box known to
	// be roomy and above one known to be nearly full, because nobody knows.
	// It must not exceed CapacityHeadway, and it inherits the invariant below.
	NeutralHeadroom float64
	// OverfullPenalty is taken, scaled by how far over, from a box whose
	// ESTIMATED fill says the item will not go in. An estimate never
	// excludes -- the box stays in the list for the user to overrule -- but
	// at full strength it pushes even a perfect category match below
	// MinViableScore, so the new-container suggestion appears beside it.
	OverfullPenalty float64
	AccessBonus     float64 // bonus for easy-to-reach boxes on frequently accessed categories
	EmptyBonus      float64 // an empty box can START a like-with-like group
	MinViableScore  float64 // below this, recommend a new container
	MaxCandidates   int
}

// DefaultWeights are tuned for sensible first-run behavior.
func DefaultWeights() Weights {
	return Weights{
		CategoryMatch:   5.0,
		CapacityHeadway: 1.5,
		NeutralHeadroom: 0.75, // CapacityHeadway at half full
		OverfullPenalty: 2.5,
		AccessBonus:     1.0,
		// An empty box also never scores below MinViableScore when the item
		// fits in it (see Recommend): telling someone to buy a container while
		// empty ones sit in front of them is the kind of answer that loses
		// their trust for good. The bonus itself stays small enough that a box
		// already holding the same things outranks an empty one.
		EmptyBonus: 1.75,
		// Strictly greater than CapacityHeadway + AccessBonus (2.5), so no
		// combination of "roomy" and "handy" can substitute for actually
		// holding related things. The lowest score that clears it therefore
		// requires real category affinity.
		MinViableScore: 2.75,
		MaxCandidates:  5,
	}
}

// Recommend scores boxes for the item and returns ranked candidates plus a
// new-container fallback when nothing clears the bar. Deterministic: equal
// scores tie-break on box name.
func Recommend(item ItemDraft, boxes []Box, w Weights) Recommendation {
	item.Normalize()
	need := item.NeedLitres()
	measured := item.measuredDims()

	// Must be non-nil: this marshals straight to the app, which calls .map()
	// on it. A nil slice becomes JSON null and red-screens the client.
	candidates := []Candidate{}
	for _, b := range boxes {
		// A bulky item and a container are simply not for each other, in
		// either direction. This is the first thing checked because it is not
		// a matter of degree that capacity arithmetic could settle: a mower
		// does not consume units of tote, and a tote is not a smaller place to
		// stand a mower.
		if item.Bulky != b.IsArea {
			continue
		}

		// Hard exclusions, and only on FACTS. An unknown capacity or fill
		// never excludes: the same rule as the tri-state safety flags, and for
		// the same reason -- an exclusion built on absent metadata removes
		// exactly the boxes nobody has annotated, which is most of them. And
		// an estimated fill never excludes either; it lowers the score below.
		//
		// None of this applies to an area. A garage does not fill up the way
		// a container does, and pretending it had a capacity would let four
		// bicycles exhaust a building.
		if !b.IsArea && !canHold(b, need, measured) {
			continue
		}
		if item.Fragile && isFalse(b.FragileSafe) {
			continue // never place fragile into a box recorded as crush-risk
		}
		if item.WeightClass == "heavy" && isFalse(b.HeavySafe) {
			continue // heavy items never go into a box recorded as not heavy-safe
		}

		score := 0.0
		reasons := []string{}

		// Category affinity: weight by the share of the box that matches.
		total := 0
		for _, n := range b.Categories {
			total += n
		}
		// MatchCategory on the lookup, because Box.Categories keys already went
		// through it in the index: aliases only help if both sides fold the
		// same way. item.Category itself stays as the user typed it, since
		// that string is what gets written to Homebox as a tag.
		if match := b.Categories[MatchCategory(item.Category)]; match > 0 && total > 0 {
			share := float64(match) / float64(total)
			score += w.CategoryMatch * share
			reasons = append(reasons, "already holds "+item.Category)
		}

		if b.IsArea {
			// An area is scored on what it IS, because there is nothing to
			// measure: it has no lid to close and no meaningful fullness. The
			// bonus is the same size as an empty container's for a reason --
			// somewhere to stand a mower is exactly as good an answer as an
			// empty tote is for a drill, and it must clear MinViableScore on
			// its own or a bulky item gets no recommendation at all.
			score += w.EmptyBonus + w.CapacityHeadway
			// Outer first. The tie-break is deliberately small: it orders the
			// list without letting a deep area fall under MinViableScore,
			// which would leave a bulky item with no answer at all in a
			// hierarchy that happens to be several levels deep.
			score -= float64(b.AreaDepth) * 0.01
			reasons = append(reasons, "somewhere large things can stand, rather than a container")
		} else {
			empty := isEmpty(b)
			// Capacity headroom: prefer boxes that still close comfortably.
			// An empty box's fill is taken as 0 -- nothing is listed in it --
			// even when nobody has observed it.
			if b.capacityKnown() && (b.fillKnown() || empty) {
				fill := 0.0
				if b.fillKnown() {
					fill = *b.FillPct / 100
				}
				projected := fill + need/float64(*b.CapacityL)
				score += w.CapacityHeadway * math.Max(0, math.Min(1, 1-projected))
				switch {
				case b.fillKnown() && !b.fillIsFact() && projected > 1:
					// An estimate that says it will not fit is taken off the
					// score, never used to exclude: the user can see the box
					// and knows better than a sum of guesses.
					score -= w.OverfullPenalty * math.Min(1, (projected-1)/0.2)
					reasons = append(reasons, fmt.Sprintf("may be too full: about %d%% already (estimated)", pct(fill)))
				case b.fillKnown():
					reasons = append(reasons, fillReason(b))
				}
			} else {
				score += w.NeutralHeadroom
				if !empty {
					reasons = append(reasons, "how full it is is not recorded")
				}
			}

			// An empty box is not a poor match, it is an opportunity: putting
			// the first tools item into an empty container is precisely how a
			// box of tools comes to exist. Without this the engine confuses
			// "nothing here shares this category" with "nothing here fits",
			// and recommends buying a new container to a user whose shelves
			// are mostly empty. It got here, so the item fits; it clears the
			// bar whatever its access or size.
			if empty {
				score += w.EmptyBonus
				score = math.Max(score, w.MinViableScore)
				reasons = append(reasons, "empty, so it can start a group")
			}
		}

		// How easy the location is to reach.
		if IsFrequentlyAccessed(item.Category) && b.Access == "easy" {
			score += w.AccessBonus
			reasons = append(reasons, "easy to reach")
		}
		if item.Fragile {
			if isTrue(b.FragileSafe) {
				reasons = append(reasons, "recorded as safe for fragile items")
			} else {
				reasons = append(reasons, "fragile-safety not recorded for this box")
			}
		}
		if item.WeightClass == "heavy" {
			if isTrue(b.HeavySafe) {
				reasons = append(reasons, "recorded as safe for heavy items")
			} else {
				reasons = append(reasons, "heavy-safety not recorded for this box")
			}
		}

		candidates = append(candidates, Candidate{Box: b, Score: score, Reasons: reasons})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].Box.Name < candidates[j].Box.Name
	})
	if len(candidates) > w.MaxCandidates {
		candidates = candidates[:w.MaxCandidates]
	}

	rec := Recommendation{Candidates: candidates}
	if len(candidates) == 0 || candidates[0].Score < w.MinViableScore {
		// Never suggest buying a container for something that does not go in
		// one. That is the reported bug in a different hat: told there was
		// nowhere for the lawn mower, the engine would answer "make a new
		// box". What a bulky item needs is an AREA -- a garage, a storage unit
		// -- and if the user's Homebox has none, the honest answer is to say
		// so rather than to propose the wrong thing confidently.
		if !item.Bulky {
			rec.NewContainer = suggestNewContainer(item, boxes)
		} else {
			rec.NoPlace = &NoPlaceForItem{
				Reason: "This is too big for a container. Boxwright places large things in a " +
					"location that holds other locations -- a garage, a shed, a storage unit -- " +
					"and none of the locations you have chosen sits inside one. Pick a location " +
					"for it below, or add somewhere in Homebox for large items to stand.",
			}
		}
	}
	return rec
}

// canHold reports whether a container can take need litres of this item, on
// what is KNOWN. Every rule here is a fact about the box meeting a fact about
// the item, and an unknown on either side lets the box through.
func canHold(b Box, need float64, measured *Dims) bool {
	// Bigger than the whole container, even empty. 10% slack: a capacity is a
	// nominal figure and things squash.
	if b.capacityKnown() && need > float64(*b.CapacityL)*1.1 {
		return false
	}
	// Will not go through the opening, however empty.
	if measured != nil && b.InteriorCm != nil && !fits(*measured, *b.InteriorCm) {
		return false
	}
	// Seen full, or seen too full for this.
	if b.fillIsFact() {
		if *b.FillPct >= 100 {
			return false
		}
		if b.capacityKnown() && *b.FillPct/100+need/float64(*b.CapacityL) > 1.1 {
			return false
		}
	}
	return true
}

// isEmpty reports whether nothing is known to be in the box: Homebox lists
// nothing inside, and no fill says otherwise.
func isEmpty(b Box) bool {
	return b.ItemCount == 0 && !(b.fillKnown() && *b.FillPct > 0)
}

func pct(fraction float64) int { return int(math.Round(fraction * 100)) }

// fillReason says how full a box is and how that is known.
func fillReason(b Box) string {
	p := pct(*b.FillPct / 100)
	switch b.FillSource {
	case FillEstimated:
		return fmt.Sprintf("about %d%% full (estimated)", p)
	case FillLidar:
		return fmt.Sprintf("about %d%% full (measured)", p)
	}
	return fmt.Sprintf("about %d%% full", p)
}

func suggestNewContainer(item ItemDraft, boxes []Box) *NewContainerSuggestion {
	// One size up from the item so the box can start a like-with-like group.
	size := "M"
	switch item.SizeBucket {
	case "M":
		size = "L"
	case "L", "XL":
		size = "XL"
	}
	access := "deep"
	if IsFrequentlyAccessed(item.Category) {
		access = "easy"
	}
	reason := "no existing box is a good fit"
	if item.Fragile {
		reason += "; needs a fragile-safe container"
	}
	s := &NewContainerSuggestion{
		SizeBucket: size,
		Access:     access,
		// The human label, so a canonical category becomes "Books & Media 1"
		// rather than the key's "Books Media 1"; this string is the name the
		// new Homebox location is actually created with.
		Label:  CategoryLabel(item.Category) + " 1",
		Reason: reason,
	}
	if t, ok := suitableType(item, ContainerTypes(boxes)); ok {
		s.ContainerType, s.CapacityL, s.InteriorCm = t.Name, t.CapacityL, t.InteriorCm
	}
	return s
}

// suitableType picks one of the user's own container types for a new
// container: the smallest with room for twice this item -- a new container
// starts a group, so it should hold more than its first member -- else the
// largest that holds it at all. A measured item must also fit inside.
func suitableType(item ItemDraft, types []ContainerType) (ContainerType, bool) {
	need := item.NeedLitres()
	measured := item.measuredDims()
	var holds []ContainerType
	for _, t := range types { // smallest first
		if float64(t.CapacityL) < need {
			continue
		}
		if measured != nil && t.InteriorCm != nil && !fits(*measured, *t.InteriorCm) {
			continue
		}
		holds = append(holds, t)
	}
	for _, t := range holds {
		if float64(t.CapacityL) >= 2*need {
			return t, true
		}
	}
	if len(holds) > 0 {
		return holds[len(holds)-1], true
	}
	return ContainerType{}, false
}
