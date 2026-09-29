package service

import (
	"context"
	"strings"

	"github.com/yourorg/medical-expert-system/internal/model"
)

// CatalogueRow is one medicine to add to or update in the shared catalogue.
type CatalogueRow struct {
	GenericName string
	BrandName   string
	Strength    string
	DosageForm  string
	Route       string
	Schedule    string
	NAPPICode   string
	PackSize    string
	Active      bool
}

// ImportMedication adds or updates one catalogue entry. The catalogue is
// platform reference data (BR-0.10), so this is an operator's tool, run from
// the command line rather than exposed to practices.
func (r *Registry) ImportMedication(ctx context.Context, row CatalogueRow) (inserted bool, err error) {
	m := &model.Medication{
		ID: model.MustNewID(), GenericName: collapseSpaces(row.GenericName), BrandName: collapseSpaces(row.BrandName),
		Strength: collapseSpaces(row.Strength), DosageForm: strings.ToLower(collapseSpaces(row.DosageForm)),
		Route: model.Route(strings.ToLower(strings.TrimSpace(row.Route))), Schedule: strings.ToUpper(strings.TrimSpace(row.Schedule)),
		NAPPICode: strings.TrimSpace(row.NAPPICode), PackSize: collapseSpaces(row.PackSize), IsActive: row.Active,
	}
	if m.Route == "" {
		m.Route = "oral"
	}
	var v model.ValidationErrors
	if m.GenericName == "" || len(m.GenericName) > 200 {
		v.Add("generic_name", "is required (at most 200 characters)")
	}
	if len(m.BrandName) > 200 {
		v.Add("brand_name", "must be at most 200 characters")
	}
	if m.Strength == "" || len(m.Strength) > 100 {
		v.Add("strength", "is required (at most 100 characters)")
	}
	if m.DosageForm == "" || len(m.DosageForm) > 50 {
		v.Add("dosage_form", "is required (at most 50 characters)")
	}
	if !m.Route.Valid() {
		v.Add("route", "is not a recognised route")
	}
	if len(m.Schedule) > 4 {
		v.Add("schedule", "must be a scheduling status such as S3")
	}
	if len(m.NAPPICode) > 20 {
		v.Add("nappi_code", "must be at most 20 characters")
	}
	if len(m.PackSize) > 50 {
		v.Add("pack_size", "must be at most 50 characters")
	}
	if len(v) > 0 {
		return false, invalid(v)
	}
	return r.platform.UpsertMedication(ctx, m, r.now())
}
