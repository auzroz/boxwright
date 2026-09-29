package api

import (
	"context"
	"hash/fnv"
	"strconv"
	"sync"
	"time"

	"boxwright/internal/homebox"
)

// Filing the same capture twice is the failure this file exists to stop.
//
// The offline queue retries on a timeout, and a timeout is exactly the case
// where the request may have SUCCEEDED and only the response was lost. So an
// item captured on one bar of signal gets filed twice, and with a
// new-container suggestion it is worse: two identically named containers, one
// item in each.
//
// The key is per ENTRY, minted once by the client and persisted with the entry
// -- never the capture id, and never a position within it. After a partial
// failure the app re-sends the same capture with a DIFFERENT SUBSET of its
// entries, so a positional key matches entry 2 against what used to be entry 5
// and silently drops an item. Preventing a duplicate by losing a capture is a
// bad trade; every fallback below therefore fails OPEN, towards a duplicate
// somebody can delete rather than an item nobody ever sees again.
const (
	// fieldKey carries the idempotency key on the created entity. Text,
	// because the fields= query that reads it back matches text fields only.
	fieldKey = "boxwrightKey"

	// maxDedupeLookups bounds what one dedupe check may cost in detail reads,
	// and doubles as a tripwire: a server that ignored the filter hands back
	// the whole inventory, and reading all of it to discover that would be
	// slower than the duplicate it is trying to avoid.
	maxDedupeLookups = 32
)

// keyField renders the idempotency key as the custom field that stores it.
func keyField(key string) homebox.CustomField {
	return homebox.CustomField{Name: fieldKey, Type: homebox.FieldTypeText, TextValue: key}
}

// containerKeyID is the idempotency key for a new container.
//
// Derived rather than client-supplied, from the same three things that already
// decide whether two entries want the SAME container, so a resend of the
// capture that created it resolves to the same key without the client having
// to remember one. FNV-1a keeps it short enough to read in Homebox's UI and
// stable across processes; it is a dedupe key, not a security boundary.
func containerKeyID(nc newContainerRequest) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(containerKey(nc)))
	return "container-" + strconv.FormatUint(h.Sum64(), 16)
}

// filedByKey returns, for each key that already exists in Homebox, the entity
// carrying it.
//
// Two things here are the whole difficulty:
//
//   - The query CANNOT be trusted to have filtered. `fields=Name` with no "="
//     is silently ignored and returns everything, so a result far larger than
//     the number of keys means the filter did not apply. ListEntitiesByField
//     makes that shape unrepresentable, and the size check below is the second
//     line: bail rather than treat an unfiltered inventory as a set of hits.
//   - Filtered rows do NOT carry their fields. With three keys in one OR'd
//     query and two rows back, nothing in the response says which row belongs
//     to which key, so each candidate needs a detail read. Verification here
//     is attribution, not caution.
//
// Every failure returns what it has and lets the caller create. A lookup that
// cannot be completed must never be read as "already filed".
func (s *Server) filedByKey(ctx context.Context, inst *instance, keys []string, isLocation bool) map[string]homebox.Entity {
	out := map[string]homebox.Entity{}
	want := make(map[string]bool, len(keys))
	query := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" || want[k] {
			continue
		}
		want[k] = true
		query = append(query, k)
	}
	if len(query) == 0 {
		return out
	}

	rows, err := inst.hb.ListEntitiesByField(ctx, fieldKey, query, &isLocation)
	if err != nil {
		s.log.Warn("catalog: dedupe lookup failed; filing anyway", "keys", len(query), "err", err)
		return out
	}
	if len(rows) > maxDedupeLookups {
		s.log.Warn("catalog: dedupe lookup returned more rows than there are keys; "+
			"treating the filter as not applied and filing anyway",
			"keys", len(query), "rows", len(rows))
		return out
	}

	for _, row := range rows {
		detail, err := inst.hb.GetEntity(ctx, row.ID)
		if err != nil {
			s.log.Warn("catalog: could not attribute a dedupe candidate; filing anyway",
				"entity", row.ID, "err", err)
			continue
		}
		k, ok := detail.FieldSet().Text(fieldKey)
		if !ok || !want[k] {
			continue
		}
		if _, seen := out[k]; seen {
			// A key that already matched twice is a duplicate from before this
			// existed. Keep the first in list order so the answer is stable.
			continue
		}
		out[k] = detail
	}
	return out
}

// filedMemory is how long a key this process created under is remembered.
// Long enough to cover any resend racing the create (seconds) and the offline
// queue's quick retries; short enough that an item the user deletes in
// Homebox is not deduplicated against for long.
const filedMemory = 10 * time.Minute

// maxFiledMemory bounds the memory; past it the oldest half is dropped.
const maxFiledMemory = 4096

type filedEntity struct {
	entity homebox.Entity
	at     time.Time
}

// rememberFiled records that this process created entity under key. Call it
// while still holding the key's claim, the moment the create succeeds.
//
// This closes a window the claim alone left open. A request reads Homebox for
// its keys ONCE, up front, and only looks again if it had to WAIT for a claim.
// So request B can do its up-front read, request A can then create, write the
// key and release entirely, and B can take the now-free claim without having
// waited -- and create a second copy on the strength of a read that was stale
// before it was used. Found by -race on a CI runner, where timing stretches
// (TestConcurrentResendsOfOneEntryCreateOneItem). Checking this memory right
// after every claim costs no upstream call, and the claim is in-process
// anyway, which is why the deployment runs one replica.
func (inst *instance) rememberFiled(key string, entity homebox.Entity) {
	if key == "" {
		return
	}
	now := time.Now()
	inst.claimMu.Lock()
	defer inst.claimMu.Unlock()
	if inst.filed == nil {
		inst.filed = map[string]filedEntity{}
	}
	if len(inst.filed) >= maxFiledMemory {
		for k, v := range inst.filed {
			if now.Sub(v.at) > filedMemory/2 {
				delete(inst.filed, k)
			}
		}
		if len(inst.filed) >= maxFiledMemory {
			inst.filed = map[string]filedEntity{}
		}
	}
	inst.filed[key] = filedEntity{entity: entity, at: now}
}

// recentlyFiled reports an entity this process created under key within
// filedMemory. Call it after taking the key's claim.
func (inst *instance) recentlyFiled(key string) (homebox.Entity, bool) {
	inst.claimMu.Lock()
	defer inst.claimMu.Unlock()
	f, ok := inst.filed[key]
	if !ok || time.Since(f.at) > filedMemory {
		return homebox.Entity{}, false
	}
	return f.entity, true
}

// claimKey takes exclusive ownership of a dedupe key within this process.
//
// The query above cannot see a create that is still in flight, so two
// concurrent requests carrying the same key both find nothing and both create.
// This closes that window: the second waits, and is told it waited so it can
// look again and find what the first just made.
//
// Process-local by design. Two Boxwright instances against one Homebox would
// still race, and the honest answer there is the same as everywhere else in
// this file -- a duplicate, not a lost item.
func (inst *instance) claimKey(ctx context.Context, key string) (release func(), waited bool) {
	for {
		inst.claimMu.Lock()
		if inst.claims == nil {
			inst.claims = map[string]chan struct{}{}
		}
		if held, busy := inst.claims[key]; busy {
			inst.claimMu.Unlock()
			select {
			case <-held:
				waited = true
				continue // Re-contend: a third caller may have taken it.
			case <-ctx.Done():
				// Give up waiting rather than hold the request open. The
				// caller proceeds, which at worst duplicates.
				return func() {}, waited
			}
		}
		done := make(chan struct{})
		inst.claims[key] = done
		inst.claimMu.Unlock()
		var once sync.Once
		return func() {
			once.Do(func() {
				inst.claimMu.Lock()
				delete(inst.claims, key)
				inst.claimMu.Unlock()
				close(done)
			})
		}, waited
	}
}
