package builtin

import (
	"context"
	"fmt"
	"sync"

	"hop.top/flo/internal/core"
	"hop.top/kit/go/ai/judge"
)

// judgeAll judges every case with up to parallel concurrent calls, verdicts
// in case order. Unlike judge.Batch it keeps the spend of the calls that
// completed when one fails: cost is the sum over every returned verdict (the
// failed call returns none), whether or not the batch succeeded. The first
// error cancels the calls not yet started and is returned wrapped with its
// case index; a call that finishes after the cancel still counts.
func judgeAll(ctx context.Context, j judge.Judge, cs []judge.Case, parallel int, costOf func(judge.Verdict) core.Cost) ([]judge.Verdict, core.Cost, error) {
	if len(cs) == 0 {
		return nil, core.Cost{}, ctx.Err()
	}
	for i, c := range cs {
		if err := judge.Validate(c); err != nil {
			return nil, core.Cost{}, fmt.Errorf("case %d: %w", i, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, core.Cost{}, err
	}
	if parallel < 1 {
		parallel = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		cost     core.Cost
		out      = make([]judge.Verdict, len(cs))
		sem      = make(chan struct{}, parallel)
	)
dispatch:
	for i := range cs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break dispatch
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			v, err := j.Judge(ctx, cs[i])
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("case %d: %w", i, err)
					cancel()
				}
				return
			}
			out[i] = v
			cost = cost.Add(costOf(v))
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, cost, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, cost, err
	}
	return out, cost, nil
}
