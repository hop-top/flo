package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"hop.top/kit/go/runtime/job"

	"hop.top/flo/internal/core"
)

// WorkerOptions configure a Worker.
type WorkerOptions struct {
	// Pollers is how many claim loops run; each owns its WorkerID. Default 4.
	Pollers int
	// Interval is the pause between polls (and after each job). Default 250ms.
	Interval time.Duration
	// StaleInterval is how often claims of dead workers are released.
	// Default 30s.
	StaleInterval time.Duration
	// ID prefixes the WorkerIDs ("<ID>-<n>"); default "<host>-<pid>".
	ID string
	// ShutdownGrace is how long Serve lets running handlers finish after its
	// context is done before cancelling theirs. Zero waits for ever.
	ShutdownGrace time.Duration
	// Logger receives queue errors and handler failures; slog.Default when nil.
	Logger *slog.Logger
}

// Worker implements core.JobWorker over a Queue: it runs kit's Poller, N of
// them, wraps each handler so the claim is heartbeated while it runs (kit's
// Poller does not), and drains running handlers on shutdown.
type Worker struct {
	q    *Queue
	opts WorkerOptions

	mu       sync.Mutex
	handlers map[core.JobType]core.JobHandler
}

var _ core.JobWorker = (*Worker)(nil)

// NewWorker returns a worker over q.
func NewWorker(q *Queue, opts WorkerOptions) *Worker {
	if opts.Pollers < 1 {
		opts.Pollers = 4
	}
	if opts.Interval <= 0 {
		opts.Interval = 250 * time.Millisecond
	}
	if opts.StaleInterval <= 0 {
		opts.StaleInterval = 30 * time.Second
	}
	if opts.ID == "" {
		h, _ := os.Hostname()
		opts.ID = fmt.Sprintf("%s-%d", h, os.Getpid())
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Worker{q: q, opts: opts, handlers: map[core.JobType]core.JobHandler{}}
}

// Handle implements core.JobWorker. Register handlers before Serve.
func (w *Worker) Handle(t core.JobType, h core.JobHandler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[t] = h
}

// gate stops claiming at shutdown and counts the jobs in flight, from claim
// to the Complete or Fail that settles them, so Serve knows when it may stop
// the pollers without leaving a claim unsettled.
type gate struct {
	job.Service
	q       *Queue
	mu      sync.Mutex
	cond    *sync.Cond
	stopped bool
	n       int
}

func newGate(q *Queue) *gate {
	g := &gate{Service: q.eng, q: q}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gate) Claim(ctx context.Context, queue, worker string) (*job.Job, error) {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return nil, nil //nolint:nilnil // kit's "no job"
	}
	g.n++
	g.mu.Unlock()
	j, err := g.Service.Claim(ctx, queue, worker)
	if err != nil || j == nil {
		g.settle()
	}
	return j, err //nolint:wrapcheck // kit service error passes through
}

func (g *gate) Complete(ctx context.Context, id string, result any) error {
	defer g.settle()
	j, _ := g.Get(ctx, id)
	err := g.Service.Complete(ctx, id, result)
	if err == nil && j != nil {
		g.q.afterSettle(ctx, j)
	}
	return err //nolint:wrapcheck // kit service error passes through
}

func (g *gate) Fail(ctx context.Context, id string, opts job.FailOpts) error {
	defer g.settle()
	j, _ := g.Get(ctx, id)
	err := g.Service.Fail(ctx, id, opts)
	if err == nil && j != nil {
		g.q.afterSettle(ctx, j)
	}
	return err //nolint:wrapcheck // kit service error passes through
}

func (g *gate) settle() {
	g.mu.Lock()
	g.n--
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *gate) stop() {
	g.mu.Lock()
	g.stopped = true
	g.mu.Unlock()
}

func (g *gate) drain() {
	g.mu.Lock()
	for g.n > 0 {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

// Serve implements core.JobWorker. It polls until ctx is done, then waits
// for the running handlers (up to ShutdownGrace when set) before returning.
func (w *Worker) Serve(ctx context.Context) error {
	// Queue calls outlive ctx so a finishing handler can still settle its
	// job; handlers get their own context, cancelled only past the grace.
	pollCtx, cancelPoll := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelPoll()
	hctx, cancelHandlers := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelHandlers()

	w.mu.Lock()
	hm := job.HandlerMap{}
	for t, h := range w.handlers {
		hm[string(t)] = w.wrap(hctx, h)
	}
	w.mu.Unlock()

	g := newGate(w.q)
	var wg sync.WaitGroup
	for i := 1; i <= w.opts.Pollers; i++ {
		p := &job.Poller{
			Service: g, Interval: w.opts.Interval, Queue: QueueName, StaleInterval: w.opts.StaleInterval,
			WorkerID: fmt.Sprintf("%s-%d", w.opts.ID, i), Handlers: hm, Logger: w.opts.Logger,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Run(pollCtx)
		}()
	}

	<-ctx.Done()
	g.stop()
	if w.opts.ShutdownGrace > 0 {
		t := time.AfterFunc(w.opts.ShutdownGrace, cancelHandlers)
		defer t.Stop()
	}
	g.drain()
	cancelPoll()
	wg.Wait()
	return nil
}

// wrap adapts a core handler to kit's: it converts the job, keeps the claim
// alive with a heartbeat every third of the TTL while the handler runs, and
// passes the handler's error through (nil finishes the job).
func (w *Worker) wrap(hctx context.Context, h core.JobHandler) func(context.Context, job.Job) error {
	return func(_ context.Context, kj job.Job) error {
		stop := w.heartbeat(kj.ID)
		defer stop()
		info := toInfo(&kj)
		return h(hctx, info.Job)
	}
}

func (w *Worker) heartbeat(id string) (stop func()) {
	every := w.q.ClaimTTL() / 3
	if every <= 0 {
		every = time.Second
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := w.q.eng.Heartbeat(context.Background(), id); err != nil {
					w.opts.Logger.Warn("job heartbeat", "job", id, "error", err)
				}
			}
		}
	}()
	return func() { close(done); <-finished }
}
