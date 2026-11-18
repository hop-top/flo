package coretest

import (
	"sync"
	"time"

	"hop.top/flo/internal/core"
)

// DB is the shared in-memory state behind the storage ports. One mutex
// guards it all, which keeps every operation atomic (the compare-and-set and
// the FinishRun transaction of the real stores come for free) at the price of
// no parallelism, which a fake does not need.
type DB struct {
	mu    sync.Mutex
	clock core.Clock
	seq   uint64 // insertion order across all records

	tenants map[core.TenantID]*tenantData

	// The ports over the state. They share the DB and its lock.
	Tenants   *Tenants
	Runs      *RunStore
	State     *StateStore
	Flows     *FlowStore
	Approvals *ApprovalStore
	Grants    *GrantStore
	Receipts  *ReceiptStore
	Outputs   *OutputStore
	Triggers  *TriggerStore
	Groups    *Groups
}

type runRec struct {
	run core.Run
	seq uint64
}

type tenantData struct {
	runs    map[core.RunID]*runRec
	steps   map[core.RunID][]core.StepRun
	staged  map[core.RunID]map[string]any
	state   map[string]map[string]any // flow -> key -> value
	flows   map[string][]core.FlowVersion
	apprs   map[core.ApprovalID]*apprRec
	grants  []core.Grant
	rcpts   []core.Receipt
	outputs []core.Output
	trigs   map[core.TriggerID]core.TriggerRecord
	fires   []core.Fire
	planned map[plannedKey]core.PlannedFire
	resv    map[core.RunID]reservation
}

type apprRec struct {
	a   core.Approval
	seq uint64
}

type plannedKey struct {
	trigger core.TriggerID
	key     string
}

type reservation struct {
	group string
	at    time.Time
}

// NewDB returns an empty DB whose records are stamped from clock.
func NewDB(clock core.Clock) *DB {
	db := &DB{clock: clock, tenants: map[core.TenantID]*tenantData{}}
	db.Tenants = &Tenants{db}
	db.Runs = &RunStore{db}
	db.State = &StateStore{db}
	db.Flows = &FlowStore{db}
	db.Approvals = &ApprovalStore{db}
	db.Grants = &GrantStore{db}
	db.Receipts = &ReceiptStore{db}
	db.Outputs = &OutputStore{db}
	db.Triggers = &TriggerStore{db}
	db.Groups = &Groups{db}
	return db
}

// td returns the tenant's data, creating it. The caller holds db.mu.
func (db *DB) td(t core.TenantID) *tenantData {
	d, ok := db.tenants[t]
	if !ok {
		d = &tenantData{
			runs: map[core.RunID]*runRec{}, steps: map[core.RunID][]core.StepRun{},
			staged: map[core.RunID]map[string]any{}, state: map[string]map[string]any{},
			flows: map[string][]core.FlowVersion{}, apprs: map[core.ApprovalID]*apprRec{},
			trigs: map[core.TriggerID]core.TriggerRecord{}, planned: map[plannedKey]core.PlannedFire{},
			resv: map[core.RunID]reservation{},
		}
		db.tenants[t] = d
	}
	return d
}

func (db *DB) next() uint64 { db.seq++; return db.seq }
