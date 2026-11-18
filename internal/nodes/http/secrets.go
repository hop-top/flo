package httpnode

import (
	"context"
	"errors"
	"strings"

	"hop.top/kit/go/storage/secret"

	"hop.top/flo/internal/core"
)

// StoreSecrets is the core.Secrets over a kit secret store. A secret named
// `name` for a profile of a tenant is the store key
// `<Prefix><tenant>/<profile>/<name>` (an empty profile drops its segment).
type StoreSecrets struct {
	Store  secret.Store
	Prefix string
}

// Key returns the store key of a secret.
func (s StoreSecrets) Key(tenant core.TenantID, profile, name string) string {
	parts := []string{string(tenant)}
	if profile != "" {
		parts = append(parts, profile)
	}
	parts = append(parts, name)
	return s.Prefix + strings.Join(parts, "/")
}

// Get implements core.Secrets. A missing secret is secret_not_found.
func (s StoreSecrets) Get(ctx context.Context, tenant core.TenantID, profile, name string) ([]byte, error) {
	got, err := s.Store.Get(ctx, s.Key(tenant, profile, name))
	if err != nil {
		if errors.Is(err, secret.ErrNotFound) {
			return nil, core.Errorf(core.CodeSecretNotFound, "secret %q not found for profile %q", name, profile)
		}
		return nil, core.Errorf(core.CodeInternal, "secret store: %v", err)
	}
	return got.Value, nil
}

var _ core.Secrets = StoreSecrets{}
