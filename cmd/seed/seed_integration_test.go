//go:build integration

package main

import (
	"context"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/medical-expert-system/internal/db"
	"github.com/yourorg/medical-expert-system/internal/model"
)

// The seeder runs the real services end to end, so a change that breaks a
// rule it relies on shows up here rather than on someone's demo day.
func TestSeedBuildsADemoPracticeThatCannotSend(t *testing.T) {
	dsn := os.Getenv("MES_TEST_DSN")
	if dsn == "" {
		t.Skip("MES_TEST_DSN not set")
	}
	ctx := context.Background()
	conn, err := db.Open(ctx, dsn, db.Options{MaxOpen: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := db.Migrate(ctx, conn, nil); err != nil {
		t.Fatal(err)
	}
	slug := "demo-" + strings.ToLower(model.MustNewID().String()[:8])
	s := &seeder{ctx: ctx, opt: options{slug: slug, name: "Demo Clinic", days: 14, seed: 7, password: "demo-pass-2026"},
		rnd: rand.New(rand.NewSource(7)), end: time.Now().UTC().Truncate(time.Second)}
	devnull, _ := os.Open(os.DevNull)
	stdout := os.Stdout
	os.Stdout = devnull
	err = s.run(conn)
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}

	var patients, doses, answered, sent, replies int
	var settings string
	q := func(query string, dest ...any) {
		t.Helper()
		if err := conn.QueryRowContext(ctx, query, slug).Scan(dest...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	q(`SELECT COUNT(*) FROM patients p JOIN tenants t ON t.id = p.tenant_id WHERE t.slug = ?`, &patients)
	q(`SELECT COUNT(*), COALESCE(SUM(d.status IN ('taken','skipped')), 0) FROM dose_events d JOIN tenants t ON t.id = d.tenant_id WHERE t.slug = ?`, &doses, &answered)
	q(`SELECT COUNT(*) FROM notifications n JOIN tenants t ON t.id = n.tenant_id WHERE t.slug = ? AND n.status <> 'failed'`, &sent)
	q(`SELECT COUNT(*) FROM notification_replies r JOIN tenants t ON t.id = r.tenant_id WHERE t.slug = ?`, &replies)
	q(`SELECT COALESCE(settings_json, '') FROM tenants WHERE slug = ?`, &settings)
	if patients != 24 || doses < 300 || answered < 150 || sent < 300 || replies < 100 {
		t.Fatalf("thin history: %d patients, %d doses (%d answered), %d messages, %d replies", patients, doses, answered, sent, replies)
	}
	if !strings.Contains(settings, `"demo":true`) || !strings.Contains(settings, `"whatsapp_enabled":false`) {
		t.Fatalf("the demo practice can send: %s", settings)
	}
	if s.sent == 0 || len(s.logins) != 5 {
		t.Fatalf("report: %d sent, %d logins", s.sent, len(s.logins))
	}
}
