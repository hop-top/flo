package judgeeval

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"hop.top/kit/go/ai/judge"
	"hop.top/kit/go/ai/judge/eval"
)

// A decision of the inbox script's hosted decider (Jev) says in its `why` what
// Jev answered. Three shapes exist:
//
//	Jev: label <Label> conf <c>, importance <i>, noul <n>   a filing label (Needs Review when under the floor)
//	Jev: needs_jad noul <n>, importance <i>                  Jev could not settle routine handling
//
// The label Jev picked under the confidence floor is not kept (the decision
// says Needs Review), so such a row has no filing reference.
var (
	whyLabel   = regexp.MustCompile(`^Jev: label (.+?)(?: \(Jev's .+ not in account\))? conf ([0-9.]+), importance ([0-9.]+), noul ([0-9.]+)`)
	whyNeedsMe = regexp.MustCompile(`^Jev: needs_jad noul ([0-9.]+), importance ([0-9.]+)`)
)

// ImportStats says what an import kept.
type ImportStats struct {
	Rows      int // rows of the run that needed a decision
	Decided   int // of those, with a recorded Jev decision
	Entries   int // case set size
	NoLabel   int // label step: Jev's pick was lost (Needs Review) or the row was not Jev's
	Abstained int // label step: references that are an abstention (Jev: needs you)
}

// ImportRun reads a run folder of the inbox script (snapshot.json and
// decisions.json) into a case set for one step of the flow, with the Jev
// verdicts the script recorded as the references. A row is a case as the flow
// would show it to the model (the headers of inbox-rows). The importance score
// Jev gave maps to the nearest of the weigh labels, spread evenly from lowest
// to highest. A row answered by hand, or carried from an earlier run, was not
// decided by Jev in this run and is left out.
func ImportRun(dir string, q judge.Question, step string) ([]eval.Entry, ImportStats, error) {
	var st ImportStats
	var snap struct {
		Rows []row `json:"rows"`
	}
	if err := readJSON(filepath.Join(dir, "snapshot.json"), &snap); err != nil {
		return nil, st, err
	}
	var dec struct {
		Decisions []struct {
			N   int    `json:"n"`
			Why string `json:"why"`
		} `json:"decisions"`
	}
	if err := readJSON(filepath.Join(dir, "decisions.json"), &dec); err != nil {
		return nil, st, err
	}
	why := map[int]string{}
	for _, d := range dec.Decisions {
		why[d.N] = d.Why
	}
	var out []eval.Entry
	for _, r := range snap.Rows {
		if r.ManualReply != nil {
			continue
		}
		st.Rows++
		w, ok := why[r.N]
		if !ok || !strings.HasPrefix(w, "Jev: ") {
			continue
		}
		st.Decided++
		ref, ok := reference(w, q, step, &st)
		if !ok {
			continue
		}
		out = append(out, eval.Entry{
			Case:      judge.Case{State: r.state(), Questions: []judge.Question{q}},
			Reference: &eval.Reference{Labels: map[string]string{q.Name: ref}},
		})
	}
	st.Entries = len(out)
	return out, st, nil
}

func reference(why string, q judge.Question, step string, st *ImportStats) (string, bool) {
	var label string
	var imp float64
	abstain := false
	if m := whyNeedsMe.FindStringSubmatch(why); m != nil {
		imp, _ = strconv.ParseFloat(m[2], 64)
		abstain = true
	} else if m := whyLabel.FindStringSubmatch(why); m != nil {
		label = m[1]
		imp, _ = strconv.ParseFloat(m[3], 64)
	} else {
		return "", false
	}
	switch step {
	case StepWeigh:
		n := len(q.Options)
		i := int(math.Round(math.Max(0, math.Min(1, imp)) * float64(n-1)))
		return q.Options[i].Value, true
	case StepLabel:
		if abstain {
			st.Abstained++
			return judge.LabelAbstain, true
		}
		for _, o := range q.Options {
			if o.Value == label {
				return label, true
			}
		}
		st.NoLabel++ // Needs Review: the pick was lost
		return "", false
	}
	return "", false
}

// row is a row of the script's snapshot.json.
type row struct {
	N       int    `json:"n"`
	Account string `json:"account"`
	From    struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"from"`
	To            []string `json:"to"`
	Subject       string   `json:"subject"`
	Date          *string  `json:"date"`
	Flags         []string `json:"flags"`
	HasAttachment bool     `json:"has_attachment"`
	Folder        *string  `json:"folder"`
	History       *int     `json:"history"`
	Prior         []struct {
		Verdict string `json:"verdict"`
	} `json:"prior"`
	ManualReply any `json:"manual_reply"`
}

// state is the item inbox-rows gives the classify steps: headers and sender
// history, no body.
func (r row) state() map[string]any {
	domain := r.From.Email
	if i := strings.LastIndex(domain, "@"); i >= 0 {
		domain = domain[i+1:]
	}
	prior := make([]any, len(r.Prior))
	for i, p := range r.Prior {
		prior[i] = p.Verdict
	}
	flags := make([]any, len(r.Flags))
	for i, f := range r.Flags {
		flags[i] = f
	}
	s := map[string]any{
		"n": float64(r.N), "account": r.Account,
		"sender":   map[string]any{"address": r.From.Email, "name": r.From.Name, "domain": domain},
		"to_count": float64(len(r.To)), "subject": r.Subject, "date": nil,
		"flags": flags, "has_attachment": r.HasAttachment, "folder": nil, "history": nil, "prior": prior,
	}
	if r.Date != nil {
		s["date"] = *r.Date
	}
	if r.Folder != nil {
		s["folder"] = *r.Folder
	}
	if r.History != nil {
		s["history"] = float64(*r.History)
	}
	return s
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec // a run folder the caller names
	if err != nil {
		return fmt.Errorf("judgeeval: %w", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("judgeeval: %s: %w", path, err)
	}
	return nil
}
