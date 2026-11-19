package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"hop.top/flo/internal/access/grants"
	"hop.top/flo/internal/core"
)

// Contract is the id of the access provisioning contract.
const Contract = "bos.access/v1"

// DefaultMaxStaleness is the age of the applied set past which flo denies when
// the config sets none: the limit the peer profile declares.
const DefaultMaxStaleness = 24 * time.Hour

// Address prefixes of the bos ids a set names.
const (
	scopePrefix     = "bos://core.business_scopes/"
	principalPrefix = "bos://core.principals/"
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	opPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[A-Za-z][A-Za-z0-9-]*)+$`)
	addrPattern   = regexp.MustCompile(`^bos://[a-z]([a-z_]*[a-z])?\.[a-z]([a-z_]*[a-z])?/[a-z]([a-z_]*[a-z])?_[0-9a-hjkmnp-tv-z]{26}$`)
)

// Config says which bos scope flo is bound to and how bos principals map to
// flo profiles.
type Config struct {
	// Scope is the bos business scope id the tenant is bound to
	// (business_scopes_...); empty accepts the scope of the set.
	Scope string
	// Principals maps a flo profile to its bos principal id.
	Principals map[string]string
	// MaxStaleness is flo's own limit; DefaultMaxStaleness when zero. The
	// limit in force is the shorter of it and the set's own.
	MaxStaleness time.Duration
}

func (c Config) limit() time.Duration {
	if c.MaxStaleness > 0 {
		return c.MaxStaleness
	}
	return DefaultMaxStaleness
}

// Unrepresentable names a grant flo cannot express and therefore denies.
type Unrepresentable struct {
	Grant  string `json:"grant"`
	Reason string `json:"reason,omitempty"`
}

// Ack is the acknowledgement of an applied set (access-ack).
type Ack struct {
	Contract        string            `json:"contract"`
	Epoch           int64             `json:"epoch"`
	State           string            `json:"state"` // applied or replayed
	Digest          string            `json:"digest"`
	AppliedAt       string            `json:"applied_at"`
	Unrepresentable []Unrepresentable `json:"unrepresentable"`
}

// StateDoc is what flo holds for the scope now (access-state).
type StateDoc struct {
	Contract            string            `json:"contract"`
	AppliedEpoch        *int64            `json:"applied_epoch"`
	Digest              *string           `json:"digest"`
	AppliedAt           *string           `json:"applied_at"`
	MaxStalenessSeconds int64             `json:"max_staleness_seconds"`
	Unrepresentable     []Unrepresentable `json:"unrepresentable"`
}

// Held is the set flo holds: what the next delta applies to and what the
// state document reports.
type Held struct {
	Epoch     int64
	Digest    string
	AppliedAt time.Time
	// Limit is the staleness limit in force for it.
	Limit           time.Duration
	Content         map[string]any
	Unrepresentable []Unrepresentable
}

// Applier applies sets to the grants of a tenant.
type Applier struct {
	grants core.GrantStore
	clock  core.Clock
	state  State
	cfg    Config
}

// New returns an Applier writing grants to store and keeping the held set in
// state.
func New(store core.GrantStore, clock core.Clock, state State, cfg Config) *Applier {
	return &Applier{grants: store, clock: clock, state: state, cfg: cfg}
}

func refuse(code core.Code, format string, args ...any) *core.Error {
	return core.Errorf(code, format, args...)
}

// Apply reads one access set (a full set, or a delta over the one held),
// writes the operations it grants into flo's grants and answers the
// acknowledgement. Replaying the epoch held with its digest is a
// confirmation: nothing changes but the time the set's age is counted from.
func (a *Applier) Apply(ctx context.Context, tenant core.TenantID, raw []byte) (Ack, error) {
	if err := core.RequireTenant(tenant); err != nil {
		return Ack{}, err //nolint:wrapcheck // *core.Error
	}
	doc, err := decode(raw)
	if err != nil {
		return Ack{}, err
	}
	if err := a.validate(doc); err != nil {
		return Ack{}, err
	}
	held, ok, err := a.state.Load()
	if err != nil {
		return Ack{}, err
	}
	epoch := doc.epoch
	digest, _ := doc.m["digest"].(string)
	now := a.clock.Now().UTC()

	if ok && epoch < held.Epoch {
		return Ack{}, refuse(core.CodeEpochStale, "epoch %d is older than the epoch %d flo holds", epoch, held.Epoch).
			WithDetail("applied_epoch", held.Epoch)
	}
	if ok && epoch == held.Epoch {
		if digest != held.Digest {
			return Ack{}, refuse(core.CodeEpochStale, "epoch %d is held with another digest", epoch).WithDetail("applied_epoch", held.Epoch)
		}
		return a.commit(ctx, tenant, held.Content, held, "replayed", now, limitOf(a.cfg, held.Content))
	}

	content := doc.m
	if doc.mode == "delta" {
		if !ok || held.Epoch != doc.base {
			applied := int64(0)
			if ok {
				applied = held.Epoch
			}
			return Ack{}, refuse(core.CodeBaseEpochMismatch, "the delta applies to epoch %d, flo holds %d", doc.base, applied).
				WithDetail("applied_epoch", applied)
		}
		content = mergeDelta(held.Content, doc.m)
	}
	if got := ContentDigest(content); got != digest {
		return Ack{}, refuse(core.CodeDigestMismatch, "digest %s is not the content's %s", digest, got)
	}
	next := Held{Epoch: epoch, Digest: digest, Content: content}
	return a.commit(ctx, tenant, content, next, "applied", now, limitOf(a.cfg, content))
}

// commit reconciles the grants with content and keeps it as the set held.
func (a *Applier) commit(ctx context.Context, tenant core.TenantID, content map[string]any, h Held, state string, now time.Time, limit time.Duration) (Ack, error) {
	desired, unrep := a.derive(content, now, limit)
	if err := a.reconcile(ctx, tenant, desired, now); err != nil {
		return Ack{}, err
	}
	h.AppliedAt, h.Limit, h.Content, h.Unrepresentable = now, limit, content, unrep
	if err := a.state.Save(h); err != nil {
		return Ack{}, err
	}
	return Ack{Contract: Contract, Epoch: h.Epoch, State: state, Digest: h.Digest, AppliedAt: now.Format(time.RFC3339),
		Unrepresentable: unrep}, nil
}

// State reports what flo holds.
func (a *Applier) State(context.Context, core.TenantID) (StateDoc, error) {
	h, ok, err := a.state.Load()
	if err != nil {
		return StateDoc{}, err
	}
	d := StateDoc{Contract: Contract, MaxStalenessSeconds: int64(a.cfg.limit() / time.Second), Unrepresentable: []Unrepresentable{}}
	if !ok {
		return d, nil
	}
	at := h.AppliedAt.UTC().Format(time.RFC3339)
	d.AppliedEpoch, d.Digest, d.AppliedAt = &h.Epoch, &h.Digest, &at
	d.MaxStalenessSeconds = int64(h.Limit / time.Second)
	if h.Unrepresentable != nil {
		d.Unrepresentable = h.Unrepresentable
	}
	return d, nil
}

// limitOf is the staleness limit in force for a set: the shorter of flo's own
// and the one the set carries.
func limitOf(cfg Config, content map[string]any) time.Duration {
	lim := cfg.limit()
	if p, ok := content["policy"].(map[string]any); ok {
		if n, ok := p["max_staleness_seconds"].(json.Number); ok {
			if s, err := n.Int64(); err == nil && s > 0 && time.Duration(s)*time.Second < lim {
				lim = time.Duration(s) * time.Second
			}
		}
	}
	return lim
}

type setDoc struct {
	m     map[string]any
	epoch int64
	mode  string
	base  int64
}

func decode(raw []byte) (setDoc, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return setDoc{}, refuse(core.CodeSetInvalid, "the set is not a JSON object")
	}
	if _, err := dec.Token(); err == nil {
		return setDoc{}, refuse(core.CodeSetInvalid, "trailing data after the set")
	}
	d := setDoc{m: m}
	d.mode, _ = m["mode"].(string)
	d.epoch = intOf(m["epoch"])
	d.base = intOf(m["base_epoch"])
	return d, nil
}

func intOf(v any) int64 {
	n, ok := v.(json.Number)
	if !ok {
		return 0
	}
	i, err := n.Int64()
	if err != nil {
		return 0
	}
	return i
}

// validate checks what the contract requires of a set that this peer relies
// on: the contract, the epoch and mode, the digest, the scope, the lists and
// the shape of every operation grant (which carries no property flo does not
// know).
func (a *Applier) validate(d setDoc) error {
	m := d.m
	if c, _ := m["contract"].(string); c != Contract {
		return refuse(core.CodeUnsupportedContract, "contract %q is not %s", c, Contract)
	}
	if d.epoch < 1 {
		return refuse(core.CodeSetInvalid, "epoch must be a whole number from 1")
	}
	if d.mode != "full" && d.mode != "delta" {
		return refuse(core.CodeSetInvalid, "mode must be full or delta")
	}
	if d.mode == "delta" && d.base < 1 {
		return refuse(core.CodeSetInvalid, "a delta carries base_epoch")
	}
	if dg, _ := m["digest"].(string); !digestPattern.MatchString(dg) {
		return refuse(core.CodeSetInvalid, "digest must be sha256:<64 hex digits>")
	}
	scope, _ := m["scope"].(string)
	if !addrPattern.MatchString(scope) || !strings.HasPrefix(scope, scopePrefix) {
		return refuse(core.CodeSetInvalid, "scope must be a bos business scope address")
	}
	if a.cfg.Scope != "" && scope != scopePrefix+a.cfg.Scope {
		return refuse(core.CodeScopeUnknown, "the set is for %s, flo is bound to %s", scope, scopePrefix+a.cfg.Scope)
	}
	if _, ok := m["generated_at"].(string); !ok {
		return refuse(core.CodeSetInvalid, "generated_at is required")
	}
	if p, ok := m["policy"].(map[string]any); !ok || intOf(p["max_staleness_seconds"]) < 1 || intOf(p["reconcile_interval_seconds"]) < 1 {
		return refuse(core.CodeSetInvalid, "policy needs max_staleness_seconds and reconcile_interval_seconds")
	}
	for _, k := range []string{"scopes", "principals", "audiences", "restricted_types", "grants"} {
		if _, ok := m[k].([]any); !ok {
			return refuse(core.CodeSetInvalid, "%s must be a list", k)
		}
	}
	if ops, present := m["operations"]; present {
		l, ok := ops.([]any)
		if !ok {
			return refuse(core.CodeSetInvalid, "operations must be a list")
		}
		for i, o := range l {
			if err := validOperation(o); err != nil {
				return refuse(core.CodeSetInvalid, "operations[%d]: %v", i, err)
			}
		}
	}
	return nil
}

var operationKeys = map[string]bool{"id": true, "subject": true, "scope": true, "operation": true, "valid_until": true}

func validOperation(v any) error {
	o, ok := v.(map[string]any)
	if !ok {
		return errors.New("not an object")
	}
	for k := range o {
		if !operationKeys[k] {
			return fmt.Errorf("property %q is not one flo knows", k)
		}
	}
	if id, _ := o["id"].(string); id == "" {
		return errors.New("id is required")
	}
	for _, k := range []string{"subject", "scope"} {
		if s, _ := o[k].(string); !addrPattern.MatchString(s) {
			return fmt.Errorf("%s must be a bos address", k)
		}
	}
	if op, _ := o["operation"].(string); !opPattern.MatchString(op) {
		return errors.New("operation must be <namespace>.<Operation>")
	}
	if vu, present := o["valid_until"]; present && vu != nil {
		s, _ := vu.(string)
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return errors.New("valid_until must be an instant")
		}
	}
	return nil
}

// mergeDelta is the set a delta produces over the one held: principals,
// audiences, scopes and restricted types replaced whole; grants added or
// replaced by id and the removed ones dropped; operations replaced whole (a
// delta without any leaves none).
func mergeDelta(base, delta map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for _, k := range []string{"contract", "scope", "policy", "generated_at", "scopes", "principals", "audiences", "restricted_types"} {
		if v, ok := delta[k]; ok {
			out[k] = v
		}
	}
	removed := map[string]bool{}
	for _, id := range list(delta["removed"]) {
		if s, ok := id.(string); ok {
			removed[s] = true
		}
	}
	byID := map[string]any{}
	for _, g := range list(base["grants"]) {
		if m, ok := g.(map[string]any); ok {
			byID[fmt.Sprint(m["id"])] = g
		}
	}
	for _, g := range list(delta["grants"]) {
		if m, ok := g.(map[string]any); ok {
			byID[fmt.Sprint(m["id"])] = g
		}
	}
	merged := make([]any, 0, len(byID))
	for id, g := range byID {
		if !removed[id] {
			merged = append(merged, g)
		}
	}
	out["grants"] = merged
	if ops := list(delta["operations"]); len(ops) > 0 {
		out["operations"] = ops
	} else {
		delete(out, "operations")
	}
	return out
}

// want is one grant flo writes: the profile holds the capability until
// Expires.
type want struct {
	profile, capability string
	expires             time.Time
}

// derive turns the operations of a set into the grants flo holds and the
// grants it cannot express. A read grant is never expressible: flo grants
// operations only. An operation grant is expressible when its subject is a
// principal (or the members of an audience) some flo profile maps to, and its
// scope is the scope flo is bound to (a narrower scope would need a grant per
// scope, which flo does not keep). Each grant ends at the earlier of its own
// end and the staleness limit counted from now.
func (a *Applier) derive(content map[string]any, now time.Time, limit time.Duration) ([]want, []Unrepresentable) {
	unrep := []Unrepresentable{}
	for _, g := range list(content["grants"]) {
		if m, ok := g.(map[string]any); ok {
			unrep = append(unrep, Unrepresentable{Grant: fmt.Sprint(m["id"]), Reason: "flo holds operation grants only: a record read is not one"})
		}
	}
	profiles := map[string][]string{}
	for p, id := range a.cfg.Principals {
		profiles[id] = append(profiles[id], p)
	}
	for _, ps := range profiles {
		sort.Strings(ps)
	}
	active := map[string]bool{}
	for _, p := range list(content["principals"]) {
		if m, ok := p.(map[string]any); ok && m["status"] == "active" {
			active[fmt.Sprint(m["id"])] = true
		}
	}
	members := map[string][]member{}
	for _, au := range list(content["audiences"]) {
		m, ok := au.(map[string]any)
		if !ok {
			continue
		}
		for _, mm := range list(m["members"]) {
			if mp, ok := mm.(map[string]any); ok {
				mb := member{principal: fmt.Sprint(mp["principal"])}
				if s, ok := mp["valid_until"].(string); ok {
					mb.until = parseTime(s)
				}
				members[fmt.Sprint(m["id"])] = append(members[fmt.Sprint(m["id"])], mb)
			}
		}
	}
	root, _ := content["scope"].(string)
	staleAt := now.Add(limit)
	var out []want
	for _, o := range list(content["operations"]) {
		op, ok := o.(map[string]any)
		if !ok {
			continue
		}
		id, subject, scope, operation := fmt.Sprint(op["id"]), fmt.Sprint(op["subject"]), fmt.Sprint(op["scope"]), fmt.Sprint(op["operation"])
		end := staleAt
		if s, ok := op["valid_until"].(string); ok {
			if t := parseTime(s); !t.IsZero() && t.Before(end) {
				end = t
			}
		}
		if !end.After(now) {
			continue // already over: it covers nothing
		}
		if scope != root {
			unrep = append(unrep, Unrepresentable{Grant: id, Reason: "the grant covers a narrower scope than the one flo is bound to"})
			continue
		}
		var subjects []member
		if au, ok := members[subject]; ok {
			subjects = au
		} else if active[subject] {
			subjects = []member{{principal: subject}}
		} else {
			unrep = append(unrep, Unrepresentable{Grant: id, Reason: "the subject is not an active principal or audience of the set"})
			continue
		}
		mapped := false
		for _, s := range subjects {
			e := end
			if !s.until.IsZero() && s.until.Before(e) {
				e = s.until
			}
			for _, prof := range profiles[strings.TrimPrefix(s.principal, principalPrefix)] {
				mapped = true
				if e.After(now) && grants.Valid(operation) {
					out = append(out, want{profile: prof, capability: operation, expires: e})
				}
			}
		}
		if !mapped {
			unrep = append(unrep, Unrepresentable{Grant: id, Reason: "no flo profile maps to the subject"})
		}
	}
	sort.SliceStable(unrep, func(i, j int) bool { return unrep[i].Grant < unrep[j].Grant })
	return out, unrep
}

type member struct {
	principal string
	until     time.Time
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// reconcile makes the provisioned grants of the tenant exactly what the set
// grants: the wanted ones written (their end refreshed), every other
// provisioned grant dropped. A local grant of the same capability becomes
// provisioned: bos governs it from now on.
func (a *Applier) reconcile(ctx context.Context, tenant core.TenantID, desired []want, now time.Time) error {
	keep := map[[2]string]bool{}
	for _, w := range desired {
		keep[[2]string{w.profile, w.capability}] = true
		exp := w.expires
		if _, err := a.grants.SetGrant(ctx, core.Grant{
			Tenant: tenant, Profile: w.profile, Capability: w.capability, Source: core.GrantProvisioned, CreatedAt: now, ExpiresAt: &exp,
		}); err != nil {
			return err //nolint:wrapcheck // port error passes through
		}
	}
	have, err := a.grants.ListGrants(ctx, tenant, "")
	if err != nil {
		return err //nolint:wrapcheck // port error passes through
	}
	for _, g := range have {
		if g.Source == core.GrantProvisioned && !keep[[2]string{g.Profile, g.Capability}] {
			if _, err := a.grants.UnsetGrant(ctx, tenant, g.Profile, g.Capability); err != nil {
				return err //nolint:wrapcheck // port error passes through
			}
		}
	}
	return nil
}
