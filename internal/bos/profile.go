package bos

import "time"

// Contract ids of the peer profile.
const (
	ContractPeer        = "bos.peer/v1"
	ContractAccess      = "bos.access/v1"
	ContractFlo         = "bos.flo/v1"
	ContractObligations = "bos.obligations/v1"
)

// PeerContract is one contract a peer lists in its profile.
type PeerContract struct {
	ID          string   `json:"id"`
	Level       string   `json:"level"` // complete, partial or unsupported
	Unsupported []string `json:"unsupported,omitempty"`
}

// PeerProfile is what flo declares about itself (the peer-profile document of
// bos.peer/v1): printed by `flo capabilities --format json`, flo having no
// server.
type PeerProfile struct {
	Tool        string         `json:"tool"`
	ToolVersion string         `json:"tool_version"`
	Contracts   []PeerContract `json:"contracts"`
	Represents  []string       `json:"represents"`
	Can         PeerCan        `json:"can"`
	Limits      PeerLimits     `json:"limits"`
	Health      string         `json:"health"`
}

// PeerCan says what flo can do for a subject by its bos address.
type PeerCan struct {
	Erase     bool `json:"erase"`
	Hold      bool `json:"hold"`
	Anonymize bool `json:"anonymize"`
}

// PeerLimits are the numbers flo enforces.
type PeerLimits struct {
	// MaxStalenessSeconds is the age of the applied access set past which flo
	// denies what the set provisioned.
	MaxStalenessSeconds int64 `json:"max_staleness_seconds"`
}

// Profile returns flo's peer profile. flo expresses operation grants and
// their end; every other grant shape is one it denies (fail closed), so
// bos.access/v1 is partial. flo holds no record by bos address that it could
// erase or hold yet, so bos.obligations/v1 is not offered.
func Profile(version string, maxStaleness time.Duration) PeerProfile {
	return PeerProfile{
		Tool: "flo", ToolVersion: version,
		Contracts: []PeerContract{
			{ID: ContractPeer, Level: "complete"},
			{ID: ContractAccess, Level: "partial", Unsupported: []string{
				"type_grant", "field_exclusion", "direction_limit", "row_relation", "office_following", "record_grant"}},
			{ID: ContractFlo, Level: "complete"},
			{ID: ContractObligations, Level: "unsupported"},
		},
		Represents: []string{"operation_grant", "expiry"},
		Limits:     PeerLimits{MaxStalenessSeconds: int64(maxStaleness / time.Second)},
		Health:     "flo status",
	}
}
