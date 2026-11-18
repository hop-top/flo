package runlog

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"

	"hop.top/kit/go/runtime/bus"

	"hop.top/flo/internal/core"
)

// fileSink is the flo sink: it appends bus.MarshalCloudEvent lines to the
// event's run file, chosen by its tenant extension and subject.
type fileSink struct {
	dir func(core.TenantID) string

	mu     sync.Mutex
	counts map[string]*count
}

// count caches how many lines a file holds, valid while its size is what the
// sink left it at; another process appending changes the size and forces a
// recount.
type count struct {
	size int64
	n    int64
	torn bool // the file does not end in a newline
}

var _ bus.Sink = (*fileSink)(nil)

func (s *fileSink) path(tenant core.TenantID, run core.RunID) string {
	name := string(run) + ".jsonl"
	if run == "" {
		name = EngineFile
	}
	return filepath.Join(s.dir(tenant), name)
}

// next returns the sequence number the next event of the run gets.
func (s *fileSink) next(tenant core.TenantID, run core.RunID) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.state(s.path(tenant, run))
	if err != nil {
		return 0, err
	}
	return c.n + 1, nil
}

func (s *fileSink) forget(tenant core.TenantID, run core.RunID) {
	s.mu.Lock()
	delete(s.counts, s.path(tenant, run))
	s.mu.Unlock()
}

func (s *fileSink) state(path string) (*count, error) {
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		c := &count{}
		s.counts[path] = c
		return c, nil
	}
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	if c, ok := s.counts[path]; ok && c.size == fi.Size() {
		return c, nil
	}
	c, err := scan(path)
	if err != nil {
		return nil, err
	}
	s.counts[path] = c
	return c, nil
}

func scan(path string) (*count, error) {
	f, err := os.Open(path) //nolint:gosec // path is built from a validated tenant and run id
	if err != nil {
		return nil, core.Wrap(core.CodeInternal, err)
	}
	defer f.Close() //nolint:errcheck // read only
	c := &count{}
	buf := make([]byte, 64<<10)
	var last byte
	for {
		n, err := f.Read(buf)
		if n > 0 {
			c.size += int64(n)
			c.n += int64(bytes.Count(buf[:n], []byte{'\n'}))
			last = buf[n-1]
		}
		if err == io.EOF { //nolint:errorlint // sentinel
			break
		}
		if err != nil {
			return nil, core.Wrap(core.CodeInternal, err)
		}
	}
	if c.size > 0 && last != '\n' {
		c.torn, c.n = true, c.n+1 // the partial line counts as a line
	}
	return c, nil
}

// Drain implements bus.Sink.
func (s *fileSink) Drain(_ context.Context, e bus.Event) error {
	tenant, _ := e.Extensions[core.ExtTenant].(string)
	t := core.TenantID(tenant)
	if err := core.RequireTenant(t); err != nil {
		return err
	}
	line, err := bus.MarshalCloudEvent(e)
	if err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	path := s.path(t, core.RunID(e.Subject))
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.state(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // validated path
	if err != nil {
		return core.Wrap(core.CodeInternal, err)
	}
	out := append(line, '\n')
	if c.torn { // end the half-written line a crash left, so ours parses
		out = append([]byte{'\n'}, out...)
	}
	_, werr := f.Write(out)
	if werr == nil && terminalTopic(e.Topic) {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		delete(s.counts, path)
		return core.Wrap(core.CodeInternal, werr)
	}
	c.size += int64(len(out))
	c.n++
	c.torn = false
	return nil
}

// Close implements bus.Sink.
func (s *fileSink) Close() error { return nil }
