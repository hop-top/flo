package coretest

import (
	"context"
	"slices"
	"strings"
	"sync"

	"hop.top/flo/internal/core"
)

// Commands is the fake core.CommandRunner: it spawns nothing. A test scripts
// answers by argv prefix; every call is recorded. With no matching script the
// answer is exit 127 with a message on stderr, so a missing script shows up
// as a failing step rather than a hang.
type Commands struct {
	mu      sync.Mutex
	scripts []cmdScript
	calls   []core.CommandSpec
}

type cmdScript struct {
	prefix []string
	fn     func(core.CommandSpec) (core.CommandResult, error)
}

// On answers commands whose argv starts with prefix (the profile is not part
// of the match: `aps run noor -- gh api ...` matches ["gh", "api"]).
func (c *Commands) On(prefix []string, fn func(core.CommandSpec) (core.CommandResult, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scripts = append(c.scripts, cmdScript{prefix: prefix, fn: fn})
}

// Reply answers commands starting with prefix with a fixed result.
func (c *Commands) Reply(prefix []string, res core.CommandResult) {
	c.On(prefix, func(core.CommandSpec) (core.CommandResult, error) { return res, nil })
}

// Run implements core.CommandRunner.
func (c *Commands) Run(_ context.Context, spec core.CommandSpec) (core.CommandResult, error) {
	c.mu.Lock()
	c.calls = append(c.calls, clone(spec))
	var fn func(core.CommandSpec) (core.CommandResult, error)
	for _, s := range c.scripts {
		if len(spec.Argv) >= len(s.prefix) && slices.Equal(spec.Argv[:len(s.prefix)], s.prefix) {
			fn = s.fn
			break
		}
	}
	c.mu.Unlock()
	if fn == nil {
		return core.CommandResult{ExitCode: 127, Stderr: []byte("coretest: no script for " + strings.Join(spec.Argv, " "))}, nil
	}
	return fn(spec)
}

// Calls returns the commands received, in order.
func (c *Commands) Calls() []core.CommandSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return clone(c.calls)
}

// Checker is the fake core.Checker. Verdict answers a request; the default
// passes.
type Checker struct {
	mu      sync.Mutex
	Verdict func(core.CheckRequest) (core.CheckResult, error)
	calls   []core.CheckRequest
}

// Check implements core.Checker.
func (c *Checker) Check(_ context.Context, req core.CheckRequest) (core.CheckResult, error) {
	c.mu.Lock()
	c.calls = append(c.calls, clone(req))
	v := c.Verdict
	c.mu.Unlock()
	if v == nil {
		return core.CheckResult{Passed: true}, nil
	}
	return v(req)
}

// Calls returns the checks received, in order.
func (c *Checker) Calls() []core.CheckRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return clone(c.calls)
}

// Secrets is the fake core.Secrets: values set per profile and name.
type Secrets struct {
	mu   sync.Mutex
	vals map[string][]byte
}

// Put stores a secret for a profile.
func (s *Secrets) Put(profile, name string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vals == nil {
		s.vals = map[string][]byte{}
	}
	s.vals[profile+"\x00"+name] = append([]byte(nil), value...)
}

// Get implements core.Secrets.
func (s *Secrets) Get(_ context.Context, _ core.TenantID, profile, name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vals[profile+"\x00"+name]
	if !ok {
		return nil, core.Errorf(core.CodeSecretNotFound, "secret %q not found for profile %q", name, profile)
	}
	return append([]byte(nil), v...), nil
}
