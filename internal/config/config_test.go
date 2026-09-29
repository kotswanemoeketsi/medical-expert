package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearEnv unsets every MES_ variable for the duration of a test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "MES_") {
			v := os.Getenv(k)
			os.Unsetenv(k)
			t.Cleanup(func() { os.Setenv(k, v) })
		}
	}
}

func TestDefaultsInDevelopment(t *testing.T) {
	clearEnv(t)
	t.Setenv("MES_DSN", "mes:pw@tcp(127.0.0.1:3306)/mes")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != "development" || c.Addr != ":8080" || c.RequireMFA || !c.AutoMigrate ||
		c.SessionIdle != 30*time.Minute || c.SessionMax != 12*time.Hour || c.LogFormat != "text" {
		t.Fatalf("unexpected development defaults: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("development config should validate: %v", err)
	}
}

func TestProductionDefaultsAreSafe(t *testing.T) {
	clearEnv(t)
	t.Setenv("MES_ENV", "production")
	t.Setenv("MES_DSN", "mes_app:pw@tcp(db:3306)/mes")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.RequireMFA || c.AutoMigrate || c.LogFormat != "json" {
		t.Fatalf("production should default MFA on, auto-migrate off, JSON logs: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("safe production config refused: %v", err)
	}
}

func TestProductionRefusesWeakSettings(t *testing.T) {
	cases := map[string]map[string]string{
		"MFA off":              {"MES_REQUIRE_MFA": "false"},
		"long session":         {"MES_SESSION_MAX": "48h"},
		"long idle":            {"MES_SESSION_IDLE": "2h", "MES_SESSION_MAX": "12h"},
		"root database access": {"MES_DSN": "root:pw@tcp(db:3306)/mes"},
		"AI over plain http":   {"ANTHROPIC_API_KEY": "k", "MES_AI_BASE_URL": "http://proxy.local"},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("MES_ENV", "production")
			t.Setenv("MES_DSN", "mes_app:pw@tcp(db:3306)/mes")
			for k, v := range vars {
				t.Setenv(k, v)
			}
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Validate(); err == nil {
				t.Fatalf("production accepted %s", name)
			}
		})
	}
}

func TestMalformedValuesAreAllReported(t *testing.T) {
	clearEnv(t)
	t.Setenv("MES_DSN", "x")
	t.Setenv("MES_SESSION_IDLE", "thirty minutes")
	t.Setenv("MES_REQUIRE_MFA", "maybe")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "MES_SESSION_IDLE") || !strings.Contains(err.Error(), "MES_REQUIRE_MFA") {
		t.Fatalf("expected both problems reported, got %v", err)
	}
}

func TestLoadDotEnv(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "\ufeff# comment\r\nMES_ADDR=:9090\r\nexport MES_DSN='mes:p#w@tcp(127.0.0.1:3306)/mes'\r\nMES_LOG_LEVEL=debug # trailing\r\n\r\nMES_ENV=test\r\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MES_ENV", "development") // already set: must win over the file
	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("MES_ADDR"); os.Unsetenv("MES_DSN"); os.Unsetenv("MES_LOG_LEVEL") })
	if os.Getenv("MES_ADDR") != ":9090" {
		t.Errorf("MES_ADDR = %q", os.Getenv("MES_ADDR"))
	}
	if os.Getenv("MES_DSN") != "mes:p#w@tcp(127.0.0.1:3306)/mes" {
		t.Errorf("quoted value mangled: %q", os.Getenv("MES_DSN"))
	}
	if os.Getenv("MES_LOG_LEVEL") != "debug" {
		t.Errorf("trailing comment not stripped: %q", os.Getenv("MES_LOG_LEVEL"))
	}
	if os.Getenv("MES_ENV") != "development" {
		t.Errorf("the file overrode a variable that was already set")
	}
	if err := LoadDotEnv(filepath.Join(dir, "missing.env")); err != nil {
		t.Errorf("a missing file should not be an error: %v", err)
	}
	if err := os.WriteFile(path, []byte("NOT A VALID LINE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadDotEnv(path); err == nil {
		t.Error("malformed line accepted")
	}
}
