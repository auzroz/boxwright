package placement

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// Dims is a size in centimetres. After Normalize, L >= W >= H: an object's
// orientation in a photo says nothing about how it can be packed, so sizes are
// compared longest side to longest side.
type Dims struct {
	L float64 `json:"l"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

const (
	// minDimCm and maxDimCm bound a believable measurement. Below half a
	// centimetre is noise; above three metres is not something anyone puts in
	// a container, and is far more likely a model answering in millimetres.
	minDimCm = 0.5
	maxDimCm = 300
)

// Normalize sorts the sides longest first and reports whether all three are
// believable. A size that is not is dropped by the caller, never repaired:
// estimating from the size bucket is always available and never wrong in kind.
func (d Dims) Normalize() (Dims, bool) {
	s := []float64{d.L, d.W, d.H}
	for _, v := range s {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < minDimCm || v > maxDimCm {
			return Dims{}, false
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(s)))
	return Dims{L: s[0], W: s[1], H: s[2]}, true
}

// Litres is the volume of the box the object would fit in.
func (d Dims) Litres() float64 { return d.L * d.W * d.H / 1000 }

// String renders the form stored in Homebox, "70x45x38", which is also what a
// person types into Homebox's own UI.
func (d Dims) String() string {
	f := func(v float64) string { return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) }
	return f(d.L) + "x" + f(d.W) + "x" + f(d.H)
}

// ParseDims reads a size typed by a person: "70x45x38", "70 × 45 × 38",
// "70*45*38", "70 x 45 x 38 cm". Anything that is not three believable numbers
// is nil -- unknown -- rather than a guess, because a wrong interior would
// exclude items that fit.
func ParseDims(s string) *Dims {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, "cm")
	for _, sep := range []string{"×", "*", ","} {
		s = strings.ReplaceAll(s, sep, "x")
	}
	parts := strings.Split(s, "x")
	if len(parts) != 3 {
		return nil
	}
	var v [3]float64
	for i, p := range parts {
		n, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil
		}
		v[i] = n
	}
	d, ok := Dims{L: v[0], W: v[1], H: v[2]}.Normalize()
	if !ok {
		return nil
	}
	return &d
}

// NominalLitres is what an item of each size bucket is taken to occupy when
// nothing better is known.
//
// Each bucket is defined by the container it fits in -- S a shoebox (about 8
// L), M a milk crate (about 25 L), L an 18-gallon container (68 L), XL a large
// one -- so the bucket is an UPPER bound, and a typical member takes around a
// third of it. XL at 60 leaves a 100 L container room for something else,
// which is true of most things that "fill a large container" in a photo.
//
// These are estimates and never exclude on their own; an observed fill or a
// measured size is what corrects them. The app reads this map from /boxes
// rather than keeping a copy that could drift.
var NominalLitres = map[string]float64{"S": 2, "M": 8, "L": 25, "XL": 60}

// Where a fill level or an item's size came from. Only an observation or a
// measurement is a FACT that may exclude a container; an estimate may only
// lower its score.
const (
	FillObserved  = "observed"  // the user said how full it is
	FillLidar     = "lidar"     // measured from a depth photo of the open container
	FillEstimated = "estimated" // Boxwright added up what it filed since the last observation

	DimsLidar  = "lidar"  // measured from a depth photo
	DimsManual = "manual" // typed by the user
	DimsVision = "vision" // the vision model's estimate from what the object is
)

// measuredDims returns the item's size only when it is a measurement, and
// therefore fit to exclude a container it would not go into. A model's
// estimate from a photo with no scale in it never is.
func (d ItemDraft) measuredDims() *Dims {
	if d.DimensionsCm == nil {
		return nil
	}
	if d.DimensionsSource != DimsLidar && d.DimensionsSource != DimsManual {
		return nil
	}
	return d.DimensionsCm
}

// NeedLitres is how much container this capture takes: its own size when one
// is known, otherwise its bucket's nominal volume, times the quantity.
func (d ItemDraft) NeedLitres() float64 {
	qty := d.Quantity
	if qty < 1 {
		qty = 1
	}
	each := NominalLitres["M"]
	if d.DimensionsCm != nil {
		each = d.DimensionsCm.Litres()
	} else if v, ok := NominalLitres[strings.ToUpper(strings.TrimSpace(d.SizeBucket))]; ok {
		each = v
	}
	return each * float64(qty)
}

// capacityKnown reports whether someone recorded how much this container holds.
func (b Box) capacityKnown() bool { return b.CapacityL != nil && *b.CapacityL > 0 }

// fillKnown reports whether the fill level means anything. FillSource decides,
// not FillPct: 0 is a real fill, and Homebox cannot delete a field, so a fill
// that must be forgotten is cleared by emptying its source.
func (b Box) fillKnown() bool {
	if b.FillPct == nil {
		return false
	}
	switch b.FillSource {
	case FillObserved, FillLidar, FillEstimated:
		return true
	}
	return false
}

// fillIsFact reports whether the fill was observed rather than added up.
func (b Box) fillIsFact() bool {
	return b.fillKnown() && (b.FillSource == FillObserved || b.FillSource == FillLidar)
}

// TracksFill reports whether filing into this box can move its fill: both its
// capacity and its fill are known. Otherwise there is nothing to add to.
func (b Box) TracksFill() bool { return b.capacityKnown() && b.fillKnown() }

// legacyUnitLitres converts litres to the abstract units clients before the
// litres model understood: an 8-unit container was "roughly a 27-gallon" one.
const legacyUnitLitres = 12.5

// SetLegacyUnits fills CapacityUnits and UsedUnits for clients that predate
// litres, from what is actually known -- 0 and 0 when it is not, which those
// clients read as room. The engine itself never reads either.
func (b *Box) SetLegacyUnits() {
	b.CapacityUnits, b.UsedUnits = 0, 0
	if !b.capacityKnown() {
		return
	}
	b.CapacityUnits = int(math.Round(float64(*b.CapacityL) / legacyUnitLitres))
	if b.CapacityUnits < 1 {
		b.CapacityUnits = 1
	}
	if b.fillKnown() {
		b.UsedUnits = int(math.Round(*b.FillPct / 100 * float64(b.CapacityUnits)))
	}
}

// AddLitres records that litres more went into the box, in memory: the batch
// reservation that stops the second item in a photo being offered the space
// the first one took, and the cache patch after a filing.
//
// Only a known fill can be added to; an unknown one stays unknown, because a
// sum with an unknown term is unknown. The source is left alone -- within a
// batch the reservation is part of the same observation -- and a written
// update chooses its own.
func (b *Box) AddLitres(litres float64) {
	if !b.capacityKnown() || !b.fillKnown() || litres <= 0 {
		return
	}
	pct := *b.FillPct + litres/float64(*b.CapacityL)*100
	b.FillPct = &pct
}

// ContainerType is a kind of container the user has recorded, derived from the
// containers themselves -- Boxwright stores no list of its own, and ships none.
type ContainerType struct {
	Name       string `json:"name"`
	CapacityL  int    `json:"capacityL"`
	InteriorCm *Dims  `json:"interiorCm"`
	Count      int    `json:"count"` // how many containers carry this type
}

// ContainerTypes lists the distinct containerType names on containers whose
// capacity is known, each with the capacity and interior most of them record
// -- so one mistyped container does not redefine the type. Smallest first.
func ContainerTypes(boxes []Box) []ContainerType {
	type tally struct {
		count     int
		capacity  map[int]int
		interiors map[string]int
		dims      map[string]Dims
	}
	byName := map[string]*tally{}
	for _, b := range boxes {
		name := strings.TrimSpace(b.ContainerType)
		if b.IsArea || name == "" || !b.capacityKnown() {
			continue
		}
		t := byName[name]
		if t == nil {
			t = &tally{capacity: map[int]int{}, interiors: map[string]int{}, dims: map[string]Dims{}}
			byName[name] = t
		}
		t.count++
		t.capacity[*b.CapacityL]++
		if b.InteriorCm != nil {
			k := b.InteriorCm.String()
			t.interiors[k]++
			t.dims[k] = *b.InteriorCm
		}
	}
	out := make([]ContainerType, 0, len(byName))
	for name, t := range byName {
		ct := ContainerType{Name: name, Count: t.count, CapacityL: mostCommon(t.capacity)}
		if k := mostCommonString(t.interiors); k != "" {
			d := t.dims[k]
			ct.InteriorCm = &d
		}
		out = append(out, ct)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CapacityL != out[j].CapacityL {
			return out[i].CapacityL < out[j].CapacityL
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// mostCommon returns the key with the highest count, ties to the smallest key
// so the answer does not depend on map order.
func mostCommon(m map[int]int) int {
	best, bestN := 0, -1
	for k, n := range m {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}

func mostCommonString(m map[string]int) string {
	best, bestN := "", -1
	for k, n := range m {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}

// fits reports whether an object of size item goes into an interior, longest
// side against longest side, allowing 5% for measurement error and for things
// that give a little.
func fits(item, interior Dims) bool {
	const slack = 1.05
	return item.L <= interior.L*slack && item.W <= interior.W*slack && item.H <= interior.H*slack
}
