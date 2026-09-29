package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"boxwright/internal/placement"
)

// vocabularyTimeout bounds the tag listing on the capture path. The picker
// degrades to the canonical seeds rather than making a user with a photo in
// hand wait on an instance that is not answering.
const vocabularyTimeout = 10 * time.Second

// categoryView is one entry in GET /api/v1/categories.
//
// Key is what a client sends back as ItemDraft.category and what /catalog
// hands to ResolveTag, so it is the NormalizeCategory shape of a tag name --
// never an aliased one, or filing an item would create a near-duplicate of the
// user's own tag. Label is display only, and for a tag it is the name exactly
// as the user typed it.
type categoryView struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	InUse     bool   `json:"inUse"`
	ItemCount int    `json:"itemCount"`
	Canonical bool   `json:"canonical"`
}

// handleCategories serves the live category vocabulary.
//
// It deliberately cannot fail. The canonical seeds are compiled in, so there
// is always an answer, and a storage unit with no signal is exactly where the
// app most needs one: a 502 here would leave the review screen with an empty
// picker and no way to file anything. Degraded means fewer categories, never
// no response.
func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	inst, err := s.instanceFor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Counts come from the box index, which is TTL-cached and offline-tolerant
	// in its own right: past TTL this refreshes it, and a failed refresh falls
	// back to the last good snapshot. Either way it is the SAME crawl
	// /recommend already pays for, never a second one.
	boxes, _, err := inst.boxIndex(r.Context(), s.cacheTTL)
	if err != nil {
		s.log.Warn("categories: box index unavailable; counts may be missing", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"categories": s.vocabulary(r.Context(), inst, categoryCounts(boxes)),
	})
}

// categoryCounts totals how many items sit under each category across the box
// index. Keys are already MatchCategory-folded: fetchBoxes builds
// Box.Categories that way, and the engine looks them up that way.
func categoryCounts(boxes []placement.Box) map[string]int {
	counts := map[string]int{}
	for _, b := range boxes {
		for key, n := range b.Categories {
			counts[key] += n
		}
	}
	return counts
}

// vocabulary merges the user's Homebox tags with the canonical seeds and
// orders the result per the client contract.
//
// The tags come first and win: they are the real vocabulary, the thing the
// inventory is actually filed under, and the seeds exist only to give an empty
// Homebox somewhere to start. A tag on nothing is still offered -- the user
// created it deliberately, and it is often created precisely because they are
// about to file something under it.
//
// A tag listing that fails is logged and skipped rather than propagated: the
// seeds alone are a usable picker, and this function is on the capture path.
func (s *Server) vocabulary(ctx context.Context, inst *instance, counts map[string]int) []categoryView {
	out := []categoryView{}
	// Two indexes, because they answer different questions. seen is exact-key
	// dedup, so two spellings of the same tag collapse. claimed is by match
	// key, and only suppresses a SEED whose meaning a tag already covers -- it
	// is never used to drop one of the user's tags in favour of another, since
	// an alias joining two tags they deliberately keep apart must not make
	// either disappear from the picker.
	seen := map[string]bool{}
	claimed := map[string]bool{}

	tags, err := inst.hb.ListTags(ctx)
	if err != nil {
		s.log.Warn("categories: listing Homebox tags failed; serving seeds and counted categories", "err", err)
	}
	for _, t := range tags {
		key := placement.NormalizeCategory(t.Name)
		if seen[key] {
			continue
		}
		seen[key] = true
		claimed[placement.MatchCategory(key)] = true
		label := strings.TrimSpace(t.Name)
		if label == "" {
			label = placement.CategoryLabel(key)
		}
		out = append(out, categoryEntry(key, label, counts))
	}

	for _, c := range placement.CanonicalCategories() {
		if seen[c.Key] || claimed[placement.MatchCategory(c.Key)] {
			continue
		}
		seen[c.Key] = true
		out = append(out, categoryEntry(c.Key, c.Label, counts))
	}

	// Anything the index counted that neither list explains. Normally empty:
	// box categories are derived from tag names, so a tag covers each one.
	// It is not empty when the tag listing just failed, and those categories
	// are the most real thing we have -- items are filed under them right now.
	leftover := make([]string, 0)
	for key := range counts {
		if !seen[key] && !claimed[key] {
			leftover = append(leftover, key)
		}
	}
	sort.Strings(leftover) // map order must not leak into the response
	for _, key := range leftover {
		seen[key] = true
		out = append(out, categoryEntry(key, placement.CategoryLabel(key), counts))
	}

	sortCategories(out)
	return out
}

// categoryEntry fills in the derived half of a row.
//
// The count is looked up by MATCH key while the row is identified by its
// normalized key, because Box.Categories is match-keyed. That is what lets a
// user tag "Books" report the items filed under the seed "books-media".
func categoryEntry(key, label string, counts map[string]int) categoryView {
	n := counts[placement.MatchCategory(key)]
	return categoryView{
		Key:       key,
		Label:     label,
		InUse:     n > 0,
		ItemCount: n,
		Canonical: placement.IsCanonicalCategory(key),
	}
}

// sortCategories applies the client contract: what the inventory actually uses
// first, heaviest first, then everything else alphabetically.
//
// Ordering is not cosmetic here. This list is read out to the vision model as
// well as rendered in the picker, and putting the categories this user really
// files things under at the front is what makes the model reach for one of
// them before inventing something new.
//
// Unused user tags sort in alphabetically among the unused seeds rather than
// ahead of them: with no usage to separate them, both are equally speculative,
// and one alphabetical block is predictable to scan.
func sortCategories(rows []categoryView) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.InUse != b.InUse {
			return a.InUse
		}
		if a.InUse && a.ItemCount != b.ItemCount {
			return a.ItemCount > b.ItemCount
		}
		return a.Key < b.Key // total order, so the response never flaps
	})
}

// identifyVocabulary is the category list handed to the vision model.
//
// Unlike handleCategories this never REFRESHES the box index, only reads what
// is already cached. A rebuild is one list call plus two per chosen location
// (eighteen seconds for eighty of them against the live instance), and making
// the user wait that long with a photo on screen buys nothing: the counts only
// order the list. The refresh happens
// moments later on /recommend, where its result is actually needed.
//
// It always returns something. Falling back to the seeds keeps identification
// working when Homebox is unreachable, which matters because a capture is the
// one step the user cannot cheaply repeat -- they may have already put the
// item in the box.
func (s *Server) identifyVocabulary(ctx context.Context, inst *instance) []placement.Category {
	// A Homebox that is refusing connections fails instantly, but one that is
	// merely hanging would otherwise hold the capture for the client's full
	// 30-second timeout before we fall back to the seeds we already had.
	ctx, cancel := context.WithTimeout(ctx, vocabularyTimeout)
	defer cancel()

	views := s.vocabulary(ctx, inst, categoryCounts(inst.cachedBoxes()))
	if len(views) == 0 {
		// Unreachable in practice -- vocabulary() always emits the seeds -- but
		// an empty vocabulary would silently strip the list from the prompt.
		return placement.CanonicalCategories()
	}
	out := make([]placement.Category, 0, len(views))
	for _, v := range views {
		out = append(out, placement.Category{
			Key:   v.Key,
			Label: v.Label,
			// Advisory for a user-invented category: the flag is only known
			// for keys we ship, and false is the honest answer for the rest.
			FrequentlyAccessed: placement.IsFrequentlyAccessed(v.Key),
		})
	}
	return out
}

// cachedBoxes returns the box index as it stands, without triggering a
// refresh. Callers that must not block on Homebox use this instead of
// boxIndex.
func (inst *instance) cachedBoxes() []placement.Box {
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	return inst.boxes
}
