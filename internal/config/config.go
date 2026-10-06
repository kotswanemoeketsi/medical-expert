// Package config reads configuration from the environment (and, for
// convenience on Windows, from a .env file) and refuses unsafe production
// settings at start-up.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Config is the whole process configuration.
type Config struct {
	Env       string // development | test | production
	Addr      string
	DSN       string
	LogLevel  slog.Level
	LogFormat string // text | json

	SessionIdle      time.Duration
	SessionMax       time.Duration
	MFAChallengeTTL  time.Duration
	MaxLoginAttempts int
	Lockout          time.Duration
	RequireMFA       bool
	MFAIssuer        string

	TrustProxyHeaders bool
	LoginPerMinute    int
	AutoMigrate       bool

	DBMaxOpen     int
	DBMaxIdle     int
	DBConnMaxLife time.Duration
	DBConnMaxIdle time.Duration

	// Note drafting. An empty key switches it off.
	AnthropicAPIKey string
	AIModel         string
	AIBaseURL       string
	AITimeout       time.Duration
	AIDailyLimit    int

	DefaultReviewMonths int

	// WhatsApp Cloud API. Messaging is off unless both the phone number ID
	// and the access token are set.
	WhatsAppPhoneNumberID string
	WhatsAppAccessToken   string
	WhatsAppAppSecret     string
	WhatsAppVerifyToken   string
	WhatsAppAPIVersion    string
	WhatsAppBaseURL       string
	WhatsAppLanguage      string

	DoseGrace              time.Duration
	ReminderFollowUp       time.Duration // 0: no follow-up
	NotificationDailyLimit int
}

// WhatsAppConfigured reports whether messages can be sent at all.
func (c *Config) WhatsAppConfigured() bool {
	return c.WhatsAppPhoneNumberID != "" && c.WhatsAppAccessToken != ""
}

func (c *Config) Production() bool { return c.Env == "production" }

// Load reads the environment. Every malformed value is reported at once.
func Load() (*Config, error) {
	var errs []string
	env := strings.ToLower(get("MES_ENV", "development"))
	switch env {
	case "development", "test", "production":
	default:
		errs = append(errs, fmt.Sprintf("MES_ENV=%q: must be development, test or production", env))
	}
	prod := env == "production"

	c := &Config{
		Env:       env,
		Addr:      get("MES_ADDR", ":8080"),
		DSN:       os.Getenv("MES_DSN"),
		LogFormat: strings.ToLower(get("MES_LOG_FORMAT", map[bool]string{true: "json", false: "text"}[prod])),
		MFAIssuer: get("MES_MFA_ISSUER", "Medical Expert System"),
	}
	c.SessionIdle = duration("MES_SESSION_IDLE", 30*time.Minute, &errs)
	c.SessionMax = duration("MES_SESSION_MAX", 12*time.Hour, &errs)
	c.MFAChallengeTTL = duration("MES_MFA_CHALLENGE_TTL", 5*time.Minute, &errs)
	c.MaxLoginAttempts = integer("MES_MAX_LOGIN_ATTEMPTS", 5, &errs)
	c.Lockout = duration("MES_LOCKOUT", 15*time.Minute, &errs)
	c.RequireMFA = boolean("MES_REQUIRE_MFA", prod, &errs)
	c.TrustProxyHeaders = boolean("MES_TRUST_PROXY_HEADERS", false, &errs)
	c.LoginPerMinute = integer("MES_LOGIN_PER_MINUTE", 10, &errs)
	c.AutoMigrate = boolean("MES_AUTO_MIGRATE", !prod, &errs)
	c.DBMaxOpen = integer("MES_DB_MAX_OPEN", 25, &errs)
	c.DBMaxIdle = integer("MES_DB_MAX_IDLE", 10, &errs)
	c.DBConnMaxLife = duration("MES_DB_CONN_MAX_LIFE", 5*time.Minute, &errs)
	c.DBConnMaxIdle = duration("MES_DB_CONN_MAX_IDLE", 2*time.Minute, &errs)
	c.AnthropicAPIKey = os.Getenv("ANTHROPIC_API_KEY")
	c.AIModel = get("MES_AI_MODEL", "")
	c.AIBaseURL = get("MES_AI_BASE_URL", "https://api.anthropic.com")
	c.AITimeout = duration("MES_AI_TIMEOUT", 90*time.Second, &errs)
	c.AIDailyLimit = integer("MES_AI_DAILY_LIMIT", 200, &errs)
	c.DefaultReviewMonths = integer("MES_DEFAULT_REVIEW_MONTHS", 6, &errs)
	c.WhatsAppPhoneNumberID = get("MES_WHATSAPP_PHONE_NUMBER_ID", "")
	c.WhatsAppAccessToken = get("MES_WHATSAPP_ACCESS_TOKEN", "")
	c.WhatsAppAppSecret = get("MES_WHATSAPP_APP_SECRET", "")
	c.WhatsAppVerifyToken = get("MES_WHATSAPP_VERIFY_TOKEN", "")
	c.WhatsAppAPIVersion = get("MES_WHATSAPP_API_VERSION", "v23.0")
	c.WhatsAppBaseURL = get("MES_WHATSAPP_BASE_URL", "https://graph.facebook.com")
	c.WhatsAppLanguage = get("MES_WHATSAPP_LANGUAGE", "en")
	c.DoseGrace = duration("MES_DOSE_GRACE", 12*time.Hour, &errs)
	c.ReminderFollowUp = durationOrOff("MES_REMINDER_FOLLOW_UP", time.Hour, &errs)
	c.NotificationDailyLimit = integer("MES_NOTIFICATION_DAILY_LIMIT", 1000, &errs)

	switch strings.ToLower(get("MES_LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn", "warning":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		errs = append(errs, "MES_LOG_LEVEL: must be debug, info, warn or error")
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		errs = append(errs, "MES_LOG_FORMAT: must be text or json")
	}
	if c.DSN == "" {
		errs = append(errs, "MES_DSN is required, e.g. mes:password@tcp(127.0.0.1:3306)/mes")
	}
	if c.MaxLoginAttempts < 1 {
		errs = append(errs, "MES_MAX_LOGIN_ATTEMPTS must be at least 1")
	}
	if c.SessionIdle > c.SessionMax {
		errs = append(errs, "MES_SESSION_IDLE cannot exceed MES_SESSION_MAX")
	}
	if c.AIDailyLimit < 1 {
		errs = append(errs, "MES_AI_DAILY_LIMIT must be at least 1")
	}
	if c.DefaultReviewMonths < 1 || c.DefaultReviewMonths > 24 {
		errs = append(errs, "MES_DEFAULT_REVIEW_MONTHS must be between 1 and 24")
	}
	if (c.WhatsAppPhoneNumberID == "") != (c.WhatsAppAccessToken == "") {
		errs = append(errs, "MES_WHATSAPP_PHONE_NUMBER_ID and MES_WHATSAPP_ACCESS_TOKEN must be set together")
	}
	if c.DoseGrace < time.Hour || c.DoseGrace > 72*time.Hour {
		errs = append(errs, "MES_DOSE_GRACE must be between 1h and 72h")
	}
	if c.ReminderFollowUp > 0 && (c.ReminderFollowUp < 10*time.Minute || c.ReminderFollowUp > 12*time.Hour) {
		errs = append(errs, "MES_REMINDER_FOLLOW_UP must be off, or between 10m and 12h")
	}
	if c.NotificationDailyLimit < 1 {
		errs = append(errs, "MES_NOTIFICATION_DAILY_LIMIT must be at least 1")
	}
	if len(errs) > 0 {
		return nil, errors.New("configuration: " + strings.Join(errs, "; "))
	}
	return c, nil
}

// Validate refuses settings that weaken the system without breaking it. The
// realistic failure is not a crash on the first request but a deployment
// running for a year with MFA quietly off, so it fails at boot instead.
func (c *Config) Validate() error {
	if !c.Production() {
		return nil
	}
	var errs []string
	if !c.RequireMFA {
		errs = append(errs, "MES_REQUIRE_MFA must be on in production (BR-1.3)")
	}
	if c.SessionMax > 24*time.Hour {
		errs = append(errs, "MES_SESSION_MAX must not exceed 24h in production")
	}
	if c.SessionIdle > time.Hour {
		errs = append(errs, "MES_SESSION_IDLE must not exceed 1h in production (BR-1.8)")
	}
	if c.AnthropicAPIKey != "" && !strings.HasPrefix(c.AIBaseURL, "https://") {
		errs = append(errs, "MES_AI_BASE_URL must use https in production: dictations are patient data")
	}
	if c.WhatsAppConfigured() && c.WhatsAppAppSecret == "" {
		errs = append(errs, "MES_WHATSAPP_APP_SECRET is required when WhatsApp is configured: without it patients' replies cannot be verified")
	}
	if c.WhatsAppConfigured() && !strings.HasPrefix(c.WhatsAppBaseURL, "https://") {
		errs = append(errs, "MES_WHATSAPP_BASE_URL must use https in production: messages carry patients' names and numbers")
	}
	if cfg, err := mysql.ParseDSN(c.DSN); err == nil && strings.EqualFold(cfg.User, "root") {
		errs = append(errs, "MES_DSN must not use the root account in production; create an application user")
	}
	if len(errs) > 0 {
		return errors.New("refusing to start: " + strings.Join(errs, "; "))
	}
	return nil
}

func get(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func duration(name string, def time.Duration, errs *[]string) time.Duration {
	v := get(name, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		*errs = append(*errs, fmt.Sprintf("%s=%q: expected a duration such as 30m or 12h", name, v))
		return def
	}
	return d
}

// durationOrOff is duration, with 0 or "off" meaning none.
func durationOrOff(name string, def time.Duration, errs *[]string) time.Duration {
	switch strings.ToLower(get(name, "")) {
	case "":
		return def
	case "0", "off", "none", "false":
		return 0
	}
	return duration(name, def, errs)
}

func integer(name string, def int, errs *[]string) int {
	v := get(name, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q: expected a whole number", name, v))
		return def
	}
	return n
}

func boolean(name string, def bool, errs *[]string) bool {
	v := strings.ToLower(get(name, ""))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	*errs = append(*errs, fmt.Sprintf("%s=%q: expected true or false", name, v))
	return def
}

// LoadDotEnv reads KEY=VALUE lines from path into the environment without
// overriding variables that are already set, so real environment settings
// always win. A missing file is not an error.
//
// Setting environment variables in PowerShell is tedious and easy to get
// wrong, so a .env file next to the binary is the simplest way to run
// locally on Windows.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	lineNo := 0
	var bad []string
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			bad = append(bad, strconv.Itoa(lineNo))
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i]) // trailing comment on an unquoted value
		}
		if _, set := os.LookupEnv(key); !set {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s: could not read line(s) %s; expected KEY=VALUE", path, strings.Join(bad, ", "))
	}
	return nil
}

// EnvFile is the .env path to load: MES_ENV_FILE if set, else ".env".
func EnvFile() string {
	if p := os.Getenv("MES_ENV_FILE"); p != "" {
		return p
	}
	return ".env"
}
