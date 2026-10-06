package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

// PracticeRepository reads and updates the practice's own row. The tenants
// table has no tenant_id column, because each row is a tenant, so these
// statements scope themselves with "id = :tenant": a practice can only ever
// reach its own row.
type PracticeRepository struct {
	scope *tenant.Scope
}

func NewPracticeRepository(s *tenant.Scope) *PracticeRepository { return &PracticeRepository{scope: s} }

const selectOwnPractice = `SELECT ` + tenantColumns + ` FROM tenants WHERE id = :tenant`

func (r *PracticeRepository) Get(ctx context.Context) (*model.Tenant, error) {
	return scanTenant(r.scope.QueryRow(ctx, selectOwnPractice))
}

const updateOwnPractice = `
UPDATE tenants
   SET name = ?, practice_number = ?, address = ?, phone = ?, email = ?, settings_json = ?, updated_at = ?
 WHERE id = :tenant`

// Update writes the practice's details and settings.
func (r *PracticeRepository) Update(ctx context.Context, t *model.Tenant, now time.Time) error {
	settings, err := json.Marshal(t.Settings)
	if err != nil {
		return err
	}
	return expectFound(r.scope.Exec(ctx, updateOwnPractice, t.Name, nullString(t.PracticeNumber),
		nullString(t.Address), nullString(t.Phone), nullString(t.Email), string(settings), now))
}
