package admission_test

import (
	"context"
	"strings"
	"testing"

	"hop.top/flo/internal/access/admission"
	"hop.top/flo/internal/access/digest"
	"hop.top/flo/internal/access/grants"
	"hop.top/flo/internal/core"
	"hop.top/flo/internal/core/coretest"
	"hop.top/flo/spec/node"
)

const tenant = core.DefaultTenant

type rig struct {
	adm *admission.Service
	gr  *grants.Service
	db  *coretest.DB
}

func newRig(t *testing.T) rig {
	t.Helper()
	fx := coretest.New()
	gr := grants.New(fx.DB.Grants, fx.Clock)
	return rig{adm: admission.New(gr, fx.DB.Receipts, fx.IDs, fx.Clock), gr: gr, db: fx.DB}
}

func request() core.AdmissionRequest {
	return core.AdmissionRequest{
		Tenant: tenant, Agent: "noor", Principal: "jad",
		Purpose: core.Purpose{Flow: "triage", Version: "1", StepID: "apply"},
		Run:     "run_1", StepRun: "stp_1",
		Operation:    "inbox/apply@1",
		Capabilities: []string{"mail.send", "mail.read"},
		SideEffect:   node.SideEffectIrreversible,
		Input:        map[string]any{"id": "m1", "n": 2},
	}
}

func TestAllowRecordsReceipt(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for _, c := range []string{"mail.send", "mail.read"} {
		if _, err := r.gr.Set(ctx, tenant, "noor", c); err != nil {
			t.Fatal(err)
		}
	}
	dec, err := r.adm.Admit(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if dec.Result != core.AdmissionAllow || dec.Reason != "" {
		t.Fatalf("decision = %+v", dec)
	}
	rc := dec.Receipt
	if rc.ID == "" || rc.Backend != "local" || rc.Decision != core.AdmissionAllow || rc.Agent != "noor" ||
		rc.Principal != "jad" || rc.Operation != "inbox/apply@1" || rc.StepID != "apply" || rc.Run != "run_1" ||
		rc.StepRun != "stp_1" || rc.SideEffect != node.SideEffectIrreversible || rc.At.IsZero() {
		t.Errorf("receipt = %+v", rc)
	}
	// the digest is the bos form over the operation, scope, actor and principal mapping
	want, err := digest.Request{
		Operation: "inbox.apply", Scope: "default", Actor: "noor", ActingFor: "jad",
		Input: []byte(`{"id":"m1","n":2}`),
	}.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if rc.Digest != want {
		t.Errorf("digest = %s, want %s", rc.Digest, want)
	}
	stored, err := r.db.Receipts.ListReceipts(ctx, tenant, "run_1")
	if err != nil || len(stored) != 1 || stored[0].ID != rc.ID {
		t.Fatalf("stored = %v %v", stored, err)
	}
}

func TestDenyRecordsReceiptAndNamesMissing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.gr.Set(ctx, tenant, "noor", "mail.read"); err != nil {
		t.Fatal(err)
	}
	dec, err := r.adm.Admit(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if dec.Result != core.AdmissionDeny || !strings.Contains(dec.Reason, "mail.send") || strings.Contains(dec.Reason, "mail.read") {
		t.Fatalf("decision = %+v", dec)
	}
	if dec.Receipt.Decision != core.AdmissionDeny || dec.Receipt.Reason != dec.Reason {
		t.Errorf("receipt = %+v", dec.Receipt)
	}
	stored, _ := r.db.Receipts.ListReceipts(ctx, tenant, "run_1")
	if len(stored) != 1 {
		t.Fatalf("a denial leaves %d receipts, want 1", len(stored))
	}
}

func TestDenyByDefaultAndWildcard(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	dec, _ := r.adm.Admit(ctx, request())
	if dec.Result != core.AdmissionDeny {
		t.Fatalf("no grants: %+v", dec)
	}
	if _, err := r.gr.SeedOwner(ctx, tenant, "noor"); err != nil {
		t.Fatal(err)
	}
	req := request()
	req.StepRun = "stp_2" // a new step run: decided afresh
	dec, err := r.adm.Admit(ctx, req)
	if err != nil || dec.Result != core.AdmissionAllow {
		t.Fatalf("wildcard: %+v %v", dec, err)
	}
}

func TestNoCapabilitiesAllowedNoAgentDenied(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	req := request()
	req.Capabilities = nil
	if dec, _ := r.adm.Admit(ctx, req); dec.Result != core.AdmissionAllow {
		t.Errorf("a step with no capabilities: %+v", dec)
	}
	req = request()
	req.Agent, req.StepRun = "", "stp_9"
	if dec, _ := r.adm.Admit(ctx, req); dec.Result != core.AdmissionDeny {
		t.Errorf("no agent: %+v", dec)
	}
}

// Asking again for the same (run, step run, digest) returns the receipt's
// current decision: grants changed since do not rewrite it, and a deferred
// decision a backend resolved is read back.
func TestSameRequestSameReceipt(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	first, _ := r.adm.Admit(ctx, request())
	if first.Result != core.AdmissionDeny {
		t.Fatal("setup: want deny")
	}
	if _, err := r.gr.SeedOwner(ctx, tenant, "noor"); err != nil {
		t.Fatal(err)
	}
	again, err := r.adm.Admit(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if again.Receipt.ID != first.Receipt.ID || again.Result != core.AdmissionDeny {
		t.Fatalf("again = %+v, first = %+v", again, first)
	}
	// a backend decides a deferred receipt in place
	rc := first.Receipt
	rc.Decision = core.AdmissionAllow
	if err := r.db.Receipts.PutReceipt(ctx, rc); err != nil {
		t.Fatal(err)
	}
	third, _ := r.adm.Admit(ctx, request())
	if third.Result != core.AdmissionAllow || third.Receipt.ID != first.Receipt.ID {
		t.Fatalf("third = %+v", third)
	}
	// a different input is a different request
	req := request()
	req.Input = map[string]any{"id": "m2"}
	other, _ := r.adm.Admit(ctx, req)
	if other.Receipt.ID == first.Receipt.ID {
		t.Error("a different input reused the receipt")
	}
	if all, _ := r.db.Receipts.ListReceipts(ctx, tenant, "run_1"); len(all) != 2 {
		t.Errorf("receipts = %d, want 2", len(all))
	}
}

func TestRejectsBadTenant(t *testing.T) {
	r := newRig(t)
	req := request()
	req.Tenant = "Bad Tenant"
	if _, err := r.adm.Admit(context.Background(), req); !core.IsCode(err, core.CodeUsage) {
		t.Errorf("err = %v", err)
	}
}
