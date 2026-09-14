package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
)

// Prefix namespaces every key this project writes.
//
// It makes the project's keys separable in a shared Redis instance, and it
// makes `SCAN pl:*` an answer to "what is this service storing".
const Prefix = "pl:"

// SchemaVersion is the shape of everything cached under a summary key.
//
// It is part of the key, not part of the value. When the cached JSON gains a
// field, a deploy would otherwise read old entries that cannot satisfy the new
// shape. With the version in the key, every pre-deploy entry simply misses and
// expires on its own -- no cache flush, no deploy-ordering problem. This is the
// structural fix for the most common cache bug there is.
const SchemaVersion = 1

// liveKey holds an outing's running counters.
func liveKey(sessionID uuid.UUID) string {
	return fmt.Sprintf("%ssession:%s:live", Prefix, sessionID)
}

// generationKey holds the counter that invalidates an athlete's rollups.
func generationKey(athleteID uuid.UUID) string {
	return fmt.Sprintf("%sathlete:%s:gen", Prefix, athleteID)
}

// analyticsKey addresses one cached rollup.
//
// The generation and the query variant are both in the key, and both have to
// be. The design named a single key per athlete, invalidated by deleting it --
// which was correct while the endpoint had one shape. It has several: the
// window and the requested sections change the payload, so one athlete has
// several live entries, and deleting one key would leave the rest stale while
// looking like invalidation had happened.
//
// Bumping a generation counter invalidates every variant at once, in a single
// INCR, without ever scanning the keyspace for keys to delete.
func analyticsKey(athleteID uuid.UUID, generation int64, variant string) string {
	return fmt.Sprintf("%sathlete:%s:analytics:s%d:g%d:%s",
		Prefix, athleteID, SchemaVersion, generation, variant)
}

// Variant reduces a request's parameters to a short, stable key fragment.
//
// Hashed rather than concatenated so the key length does not depend on what
// the caller passes, and so a parameter containing a colon cannot forge a key
// boundary.
func Variant(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte("|"))
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
