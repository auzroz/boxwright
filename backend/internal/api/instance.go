package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"boxwright/internal/homebox"
	"boxwright/internal/placement"
)

// One backend, several Homeboxes.
//
// The self-hosted case is one instance configured from the environment, and
// that stays the default: somebody running this beside their own Homebox
// should not have to type a token into an app. But a signed build handed to
// another person cannot work that way -- their items would land in the
// operator's inventory -- so a client may bring its own Homebox URL and token,
// and everything cached about a Homebox is scoped to the credentials that
// reached it.
//
// Client credentials are refused unless ALLOW_CLIENT_HOMEBOX is set. A backend
// that has not been told to accept them will never fetch a URL a client named,
// which is the honest way to describe the SSRF surface this opens: an
// authenticated caller can otherwise point the server at any address it can
// reach. The /api shared token is the gate in front of that, and config.Load
// already refuses to bind beyond loopback without one.
const (
	// headerHomeboxURL and headerHomeboxToken carry per-request credentials.
	// The token alone is enough, and is the safer half: it reuses the
	// operator's configured URL, so it cannot name a host.
	headerHomeboxURL   = "X-Boxwright-Homebox-Url"
	headerHomeboxToken = "X-Boxwright-Homebox-Token"

	// maxInstances bounds the cache. Each instance holds one box index; a
	// backend serving many people must not grow one per person forever. The
	// least recently used is evicted, which costs that person one rebuild.
	maxInstances = 64
)

// errClientCredentials is returned when a request carries Homebox credentials
// this backend was not configured to accept.
var errClientCredentials = errors.New(
	"this Boxwright is configured for a single Homebox and ignores per-request credentials; " +
		"set ALLOW_CLIENT_HOMEBOX=true to accept them")

// instance is one Homebox and everything this process caches about it.
//
// The box index used to live on the Server as a single set of fields. It could
// not stay there the moment two callers could mean two different Homeboxes:
// one person's containers would be served to another, and a catalog write
// against one inventory would patch the cache of the other.
type instance struct {
	hb *homebox.Client
	// key identifies the credentials this was built for, so the same caller
	// gets the same cache and a different one never does.
	key string

	mu      sync.RWMutex
	boxes   []placement.Box
	fetched time.Time
	// generation is bumped by invalidate() and by patchBox(). A refresh
	// records the generation it started from and refuses to commit if it
	// changed while it ran -- otherwise a catalog write landing mid-rebuild is
	// silently overwritten by a snapshot taken before it, and the just-filed
	// item stays invisible for a whole TTL while the response claims
	// stale=false.
	generation uint64
	// loaded records that a fetch has succeeded at least once. It is
	// deliberately separate from fetched, which invalidate() zeroes: an
	// invalidated cache still HAS data to fall back on if the next refresh
	// fails, whereas a cold start has none.
	loaded bool
	// used orders eviction. Touched on every lookup, under the Server's lock.
	used time.Time
	// lastRead is the wall clock of the most recent REQUEST for the box index,
	// in UnixNano; zero means never read. It gates the change-feed watcher's
	// pre-warm, so that an idle backend nobody is using makes no unprompted
	// upstream calls -- which is why the pre-warm itself does not stamp it and
	// takes instance.boxIndexUnstamped instead.
	// Atomic rather than under inst.mu because it is stamped on the read path
	// and read from the watcher's goroutine, neither of which should wait on
	// the other.
	lastRead atomic.Int64

	// fillMu serialises fill-level writes against this Homebox. A fill write
	// reads the container and writes it back (Homebox has no conditional
	// update), so two catalog requests filing into one container at once
	// would each add to the same starting fill and one addition would be
	// lost. One mutex rather than one per container: these writes are a
	// handful per capture, and the deployment is a single replica -- which
	// this lock is one more reason for.
	fillMu sync.Mutex

	// writeMu guards the self-echo suppression state below, and is
	// deliberately NOT inst.mu. The watcher takes it on every mutation frame
	// off the change feed, while inst.mu is held across a box-index commit --
	// putting the two together would queue the socket's reader behind a
	// measured ~21s rebuild.
	writeMu sync.Mutex
	// writesLive is how many upstream writes against this Homebox are in
	// flight; muteUntil holds the mute open for a moment after the last one
	// returns, because the echo of a write arrives after the write does.
	writesLive int
	muteUntil  time.Time

	// claims serialises concurrent creates carrying the same idempotency key.
	// Per instance, because two different Homeboxes can hold the same key and
	// have nothing to do with each other.
	claimMu sync.Mutex
	claims  map[string]chan struct{}
	// filed remembers what THIS process created under each key, for a
	// while; guarded by claimMu. See rememberFiled in dedupe.go.
	filed map[string]filedEntity
}

// selfWriteQuiet is how long after our own last upstream write an
// entity.mutation frame is still assumed to be the echo of it.
//
// The frame carries nothing but "something changed" -- no id, no operation --
// so our own echo is indistinguishable from somebody else's edit except by
// timing. The counter covers the write itself; this covers the gap between the
// last upstream call returning and its frame coming back. A foreign edit
// unlucky enough to land inside one of our windows is not seen, and falls back
// to the TTL that was the only mechanism before any of this existed.
const selfWriteQuiet = 3 * time.Second

// beginWrite marks an upstream write against this Homebox as in flight and
// returns the release, which the caller must defer.
//
// Homebox's change feed fires for our OWN writes as well as anyone else's, so
// without this a cataloguing session invalidates its own index once per item
// and undoes the ~4.3s per item that patchBox exists to save.
//
// A depth counter released by defer, not a timestamp stamped before the write:
// one entry can be CreateEntity plus SetFields plus a 10 MB attachment upload,
// which outlives any window picked in advance and lets its own echo through. A
// counter is unconditional for as long as any write is live, so no write can
// outrun it. It is deliberately not a count of EXPECTED events either -- the
// frames carry no identity, the number one entry produces is unknowable, and a
// single miscount would desynchronise such a counter permanently.
func (inst *instance) beginWrite() (done func()) {
	inst.writeMu.Lock()
	inst.writesLive++
	inst.writeMu.Unlock()

	// OnceFunc because a caller that both defers the release and calls it
	// early would otherwise decrement twice, and a negative count mutes
	// nothing ever again.
	return sync.OnceFunc(func() {
		inst.writeMu.Lock()
		inst.writesLive--
		inst.muteUntil = time.Now().Add(selfWriteQuiet)
		inst.writeMu.Unlock()
	})
}

// muted reports whether a change-feed event arriving at now should be read as
// the echo of our own writing rather than as somebody else's edit.
func (inst *instance) muted(now time.Time) bool {
	inst.writeMu.Lock()
	defer inst.writeMu.Unlock()
	return inst.writesLive > 0 || now.Before(inst.muteUntil)
}

// readRecently reports whether anything has asked this instance for the box
// index within ttl.
func (inst *instance) readRecently(ttl time.Duration) bool {
	ns := inst.lastRead.Load()
	return ns != 0 && time.Since(time.Unix(0, ns)) < ttl
}

// credentials are what a request resolved to.
type credentials struct {
	baseURL string
	token   string
}

// key derives the cache key. The token is hashed rather than stored: it ends
// up in a map key that could otherwise be printed by any future debug line,
// and a hash costs nothing here.
func (c credentials) key() string {
	sum := sha256.Sum256([]byte(c.baseURL + "\x00" + c.token))
	return hex.EncodeToString(sum[:16])
}

// same reports whether two credentials are identical, comparing the token in
// constant time.
func (c credentials) same(other credentials) bool {
	return c.baseURL == other.baseURL &&
		subtle.ConstantTimeCompare([]byte(c.token), []byte(other.token)) == 1
}

// credentialsFor reads the Homebox credentials one request should use.
//
// Neither header means the configured pair, which is the whole self-hosted
// product. A token alone overrides only the token. A URL alone is refused
// rather than silently paired with the operator's token: that would send the
// operator's credential to a host the caller chose.
func (s *Server) credentialsFor(r *http.Request) (credentials, error) {
	rawURL := strings.TrimSpace(r.Header.Get(headerHomeboxURL))
	token := strings.TrimSpace(r.Header.Get(headerHomeboxToken))
	if rawURL == "" && token == "" {
		return s.defaults, nil
	}
	if !s.allowClientCredentials {
		return credentials{}, errClientCredentials
	}
	if token == "" {
		return credentials{}, fmt.Errorf("%s was sent without %s; a URL alone would send this "+
			"server's own Homebox credential to a host you named", headerHomeboxURL, headerHomeboxToken)
	}
	if rawURL == "" {
		if s.defaults.baseURL == "" {
			return credentials{}, fmt.Errorf("%s is required: this server has no Homebox URL of its own",
				headerHomeboxURL)
		}
		return credentials{baseURL: s.defaults.baseURL, token: token}, nil
	}
	if err := validateHomeboxURL(rawURL); err != nil {
		return credentials{}, err
	}
	return credentials{baseURL: strings.TrimRight(rawURL, "/"), token: token}, nil
}

// validateHomeboxURL rejects anything that is not an absolute http(s) URL.
//
// Deliberately not an allowlist of hosts. Restricting which Homeboxes a user
// may reach is the operator's decision and belongs in front of this service,
// where a network policy can enforce it; a half-hearted check here would read
// as protection it does not provide. What IS enforced is the scheme, because
// url.Parse accepts file:// and gopher:// without complaint.
func validateHomeboxURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid %s: %w", headerHomeboxURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must be http or https, got %q", headerHomeboxURL, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s has no host", headerHomeboxURL)
	}
	return nil
}

// instanceFor returns the cached instance for a request's credentials,
// building one if this is the first time they have been seen.
func (s *Server) instanceFor(r *http.Request) (*instance, error) {
	creds, err := s.credentialsFor(r)
	if err != nil {
		return nil, err
	}
	return s.instanceFrom(creds), nil
}

// defaultInstance is the operator's own Homebox: the one every request
// resolves to when it brings no credentials of its own, and the only one in a
// single-Homebox deployment.
func (s *Server) defaultInstance() *instance { return s.instanceFrom(s.defaults) }

func (s *Server) instanceFrom(creds credentials) *instance {
	key := creds.key()

	s.instMu.Lock()
	defer s.instMu.Unlock()
	if s.instances == nil {
		s.instances = map[string]*instance{}
	}
	if inst, ok := s.instances[key]; ok {
		inst.used = time.Now()
		return inst
	}

	// Evict before inserting, so the map never exceeds the bound even briefly.
	for len(s.instances) >= maxInstances {
		var oldestKey string
		var oldest time.Time
		for k, inst := range s.instances {
			if oldestKey == "" || inst.used.Before(oldest) {
				oldestKey, oldest = k, inst.used
			}
		}
		// Dropping the map entry is the whole of eviction today: nothing holds
		// a pointer to an instance beyond the request using it. A per-instance
		// background goroutine would need its stop() called right here first,
		// which is why the change-feed watcher holds a KEY and resolves it
		// through lookupInstance -- that returns nil the moment this lands.
		delete(s.instances, oldestKey)
	}

	inst := &instance{
		hb:   homebox.New(creds.baseURL, creds.token),
		key:  key,
		used: time.Now(),
	}
	s.instances[key] = inst
	return inst
}

// lookupInstance resolves the key a background goroutine holds, or nil if that
// instance is gone.
//
// It CREATES nothing: a watcher naming an evicted instance must not resurrect
// a cache no request has asked for. It also deliberately leaves inst.used
// alone -- a background refresh stamping the LRU clock would pin its own
// instance and defeat maxInstances entirely.
func (s *Server) lookupInstance(key string) *instance {
	s.instMu.Lock()
	defer s.instMu.Unlock()
	return s.instances[key]
}
