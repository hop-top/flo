// Package digest computes the request digest of node.md §10.2: SHA-256 over
// the canonical JSON of {v, operation, scope, actor, acting_for, input}.
//
// It reimplements the canonical form of the bos autonomy package
// (internal/core/autonomy/digest.go) so one request has one digest in flo,
// bos and dek. The form is: a JSON object with sorted keys, the input
// re-encoded with sorted keys and exact numbers, no HTML escaping, no
// insignificant white space and no trailing newline. testdata/vectors.json
// pins it.
package digest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"hop.top/flo/internal/core"
)

// Version is the version of the canonical form. A change of the form
// changes it, so an old digest never equals a new one.
const Version = 1

// Prefix starts every digest: "sha256:" and the hex SHA-256.
const Prefix = "sha256:"

// Request is one logical request: the operation, the scope it runs in, the
// profile that runs it, the person that profile acts for and the input.
type Request struct {
	// Operation is <namespace>.<Operation>; see Operation.
	Operation string
	Scope     string
	Actor     string
	ActingFor string
	// Input is the arguments as JSON; empty is {}.
	Input json.RawMessage
}

// Canonical returns the request in canonical form.
func (r Request) Canonical() ([]byte, error) {
	in, err := canonJSON(r.Input)
	if err != nil {
		return nil, err
	}
	return marshal(map[string]any{
		"v": Version, "operation": r.Operation, "scope": r.Scope,
		"actor": r.Actor, "acting_for": r.ActingFor, "input": json.RawMessage(in),
	})
}

// Digest returns "sha256:" and the hex SHA-256 of the canonical form.
func (r Request) Digest() (string, error) {
	c, err := r.Canonical()
	if err != nil {
		return "", err
	}
	return Of(c), nil
}

// Of returns the digest of an already canonical request.
func Of(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return Prefix + hex.EncodeToString(sum[:])
}

// Input renders a Go value as the JSON input of a Request. nil is {}.
func Input(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage(`{}`), nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "digest input is not JSON: %w", err)
	}
	return b, nil
}

// Operation names the operation of a request: the capability
// when there is one (an approval item), else the node type with its major
// dropped and "/" as ".", so `inbox/apply@1` is `inbox.apply`. A name that
// has no "/" or "@" (a core kind, an operation already in that form) is
// unchanged.
func Operation(capability, nodeType string) string {
	if capability != "" {
		return capability
	}
	t, _, _ := strings.Cut(nodeType, "@")
	return strings.Replace(t, "/", ".", 1)
}

// ForRun builds the Request of a run's step: scope is the tenant, actor the
// run_as profile, acting_for the principal.
func ForRun(tenant core.TenantID, operation, actor, principal string, input any) (Request, error) {
	in, err := Input(input)
	if err != nil {
		return Request{}, err
	}
	return Request{Operation: operation, Scope: string(tenant), Actor: actor, ActingFor: principal, Input: in}, nil
}

// canonJSON re-encodes JSON with sorted keys and exact numbers. Empty
// input is {}.
func canonJSON(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte(`{}`), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("digest input: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("digest input: trailing data after the input")
	}
	return marshal(v)
}

// marshal encodes v with sorted keys, without HTML escaping and without a
// trailing newline.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("digest encode: %w", err)
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
