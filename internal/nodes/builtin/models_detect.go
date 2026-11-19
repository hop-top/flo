package builtin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"hop.top/kit/go/ai/llm"
)

// detectOllama reports whether the daemon the ollama model uri names lists
// it, and says what is missing when it does not. The error is the caller's
// to return: a cancelled context.
func (m *modelClients) detectOllama(ctx context.Context, run string, uri llm.URI) (found bool, why string, err error) {
	base := ollamaBase
	if v, ok := uri.Params["base_url"]; ok {
		if v != "" {
			base = strings.TrimRight(v, "/")
		}
	} else if uri.Host != "" {
		base = "http://" + uri.Host
	}
	ok, err := m.listed(ctx, run, base, uri.Model)
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "ollama at " + base + " is not answering or does not list " + uri.Model, nil
	}
	return true, "", nil
}

// listed asks the daemon at base (GET /api/tags, over the invocation's HTTP
// seam, so a recording keeps the call) whether it lists model. The answer is
// kept for the run: later steps of it do not ask again. A daemon that does
// not answer in time, or answers with anything but a model list, lists
// nothing.
func (m *modelClients) listed(ctx context.Context, run, base, model string) (bool, error) {
	key := run + "|" + base + "|" + model
	if run != "" {
		if v, ok := m.probes.get(key); ok {
			return v, nil
		}
	}
	v, err := m.probe(ctx, base, model)
	if err != nil {
		return false, err
	}
	if run != "" {
		m.probes.put(key, v)
	}
	return v, nil
}

func (m *modelClients) probe(ctx context.Context, base, model string) (bool, error) {
	pctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return false, nil //nolint:nilerr // a base URL that cannot be asked lists nothing
	}
	doer := m.doer(ctx)
	var resp *http.Response
	if doer == nil {
		resp, err = http.DefaultClient.Do(hreq)
	} else {
		resp, err = doer.Do(hreq)
	}
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err() //nolint:wrapcheck // the engine's to classify
		}
		return false, nil
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return false, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, detectMaxBody))
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err() //nolint:wrapcheck // the engine's to classify
		}
		return false, nil
	}
	var tags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &tags) != nil {
		return false, nil
	}
	// A name without a tag is the daemon's :latest.
	want := []string{model}
	if !strings.Contains(model, ":") {
		want = append(want, model+":latest")
	}
	for _, m := range tags.Models {
		for _, w := range want {
			if m.Name == w || m.Model == w {
				return true, nil
			}
		}
	}
	return false, nil
}

// probeCache remembers what the daemon listed, per run.
type probeCache struct {
	mu sync.Mutex
	m  map[string]bool
}

func (c *probeCache) get(k string) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *probeCache) put(k string, v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= probeCacheMax {
		c.m = map[string]bool{}
	}
	c.m[k] = v
}
