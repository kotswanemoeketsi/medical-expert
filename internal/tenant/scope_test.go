package tenant

import (
	"context"
	"testing"

	"github.com/yourorg/medical-expert-system/internal/model"
)

func TestGuardAcceptsScopedStatements(t *testing.T) {
	ok := []string{
		`SELECT id FROM patients WHERE tenant_id = :tenant AND status = ?`,
		`SELECT p.id FROM patients p WHERE p.tenant_id=:tenant AND p.last_name LIKE ?`,
		`INSERT INTO patients (tenant_id, id, first_name) VALUES (:tenant, ?, ?)`,
		// The case that breaks positional injection: a parameter before the
		// tenant predicate.
		"UPDATE users SET status = ? WHERE tenant_id = :tenant AND id = ?",
		`DELETE FROM sessions WHERE tenant_id = :tenant AND id = ?`,
		"-- lookup\nSELECT id FROM patients WHERE tenant_id = :tenant",
	}
	for _, q := range ok {
		if err := Guard(q); err != nil {
			t.Errorf("Guard rejected a valid statement: %q\n  err: %v", q, err)
		}
	}
}

func TestGuardRejectsUnscopedStatements(t *testing.T) {
	bad := []string{
		`SELECT id FROM patients`,
		`SELECT id FROM patients WHERE status = ?`,
		`INSERT INTO patients (id, first_name) VALUES (?, ?)`,
		`SELECT id FROM patients WHERE tenant_id = ?`,                 // bare placeholder
		`SELECT id FROM patients WHERE p.tenant_id=? AND x = :tenant`, // bare placeholder, unspaced
		`SELECT id FROM patients WHERE tenant_id = '0000'`,            // hardcoded literal
		"SELECT id FROM patients -- WHERE tenant_id = :tenant",        // marker only in a comment
	}
	for _, q := range bad {
		if err := Guard(q); err == nil {
			t.Errorf("Guard accepted an unscoped statement: %q", q)
		}
	}
}

func TestNewScopeRejectsZeroTenant(t *testing.T) {
	if _, err := NewScope(nil, model.ID{}); err == nil {
		t.Fatal("expected NewScope to reject a zero tenant ID")
	}
}

func TestBreakGlassRequiresReason(t *testing.T) {
	if _, err := NewBreakGlassScope(nil, model.ID{1}, BreakGlass{Reason: "  "}); err == nil {
		t.Fatal("expected break-glass access without a reason to be refused")
	}
}

func TestBindInterleavesTenantValueAtTheRightPosition(t *testing.T) {
	tid := model.ID{0xaa, 0xbb}
	s, err := NewScope(nil, tid)
	if err != nil {
		t.Fatal(err)
	}

	q, args, err := s.bind(
		`UPDATE patients SET mobile = ? WHERE tenant_id = :tenant AND id = ?`,
		"0821234567", "patient-id")
	if err != nil {
		t.Fatal(err)
	}
	if q != `UPDATE patients SET mobile = ? WHERE tenant_id = ? AND id = ?` {
		t.Fatalf("marker was not rewritten correctly: %q", q)
	}
	if len(args) != 3 {
		t.Fatalf("expected 3 args, got %d", len(args))
	}
	if args[0] != "0821234567" {
		t.Errorf("arg 0 should be the caller's first value, got %v", args[0])
	}
	if string(args[1].([]byte)) != string(tid.Bytes()) {
		t.Errorf("arg 1 should be the tenant id, got %v", args[1])
	}
	if args[2] != "patient-id" {
		t.Errorf("arg 2 should be the caller's second value, got %v", args[2])
	}
}

func TestBindHandlesMultipleMarkers(t *testing.T) {
	s, _ := NewScope(nil, model.ID{0x01})
	_, args, err := s.bind(`
		SELECT c.purpose FROM consents c
		INNER JOIN (SELECT purpose FROM consents WHERE tenant_id = :tenant AND patient_id = ?) n
		WHERE c.tenant_id = :tenant AND c.patient_id = ?`,
		"p1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 4 {
		t.Fatalf("expected 4 args for 2 markers + 2 params, got %d", len(args))
	}
}

func TestBindRejectsArgumentCountMismatch(t *testing.T) {
	s, _ := NewScope(nil, model.ID{0x01})
	if _, _, err := s.bind(`SELECT id FROM p WHERE tenant_id = :tenant AND a = ? AND b = ?`, "one"); err == nil {
		t.Fatal("expected an error when fewer arguments are supplied than placeholders")
	}
	if _, _, err := s.bind(`SELECT id FROM p WHERE tenant_id = :tenant AND a = ?`, "a", "b"); err == nil {
		t.Fatal("expected an error when more arguments are supplied than placeholders")
	}
}

func TestBindIgnoresPlaceholdersInsideQuotedLiterals(t *testing.T) {
	s, _ := NewScope(nil, model.ID{0x01})
	q, args, err := s.bind(
		`SELECT id FROM p WHERE tenant_id = :tenant AND note <> 'why?' AND tag <> 'it''s :tenant' AND a = ?`,
		"x")
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 {
		t.Fatalf("expected tenant + 1 arg, got %d", len(args))
	}
	want := `SELECT id FROM p WHERE tenant_id = ? AND note <> 'why?' AND tag <> 'it''s :tenant' AND a = ?`
	if q != want {
		t.Fatalf("quoted literal was altered:\n got %q\nwant %q", q, want)
	}
}

func TestInTxRefusesWithoutADatabase(t *testing.T) {
	s, _ := NewScope(nil, model.ID{0x01})
	called := false
	err := s.InTx(context.Background(), func(*Scope) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("InTx must refuse, without running fn, when there is no database")
	}
}
