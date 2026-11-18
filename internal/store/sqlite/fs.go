package sqlite

import (
	"os"

	"hop.top/flo/internal/core"
	"hop.top/flo/internal/store/tenants"
)

func mkdirs(r *tenants.Registry, t core.TenantID) error {
	if err := os.MkdirAll(r.RunsDir(t), 0o750); err != nil {
		return internal(err)
	}
	return nil
}
