package coretest

import (
	"strings"
	"sync"
)

// base32 alphabet of typeids (Crockford, lowercase).
const typeidAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// IDs mints sequential, valid typeids: `run_00000000000000000000000001`,
// `run_00000000000000000000000002`, ... A single counter runs across
// prefixes, so ids are unique and ordered by creation.
type IDs struct {
	mu sync.Mutex
	n  uint64
}

// New implements core.IDs.
func (i *IDs) New(prefix string) string {
	i.mu.Lock()
	i.n++
	n := i.n
	i.mu.Unlock()
	var suffix [26]byte
	for k := range suffix {
		suffix[k] = '0'
	}
	for k := 25; n > 0 && k >= 0; k-- {
		suffix[k] = typeidAlphabet[n%32]
		n /= 32
	}
	return prefix + "_" + strings.ToLower(string(suffix[:]))
}
