-- 0001_init.sql — tenancy, identity, patient record, consent and audit.
--
-- Conventions (see SYSTEM-DOCUMENTATION.md section 4.2):
--   * identifiers are BINARY(16) version-4 UUIDs
--   * timestamps are DATETIME(3) in UTC; the application supplies every value
--   * tenant_id leads every lookup index on a tenant-scoped table
--   * uniqueness is per tenant, never global
--   * child rows reference parents by (tenant_id, id), so the database itself
--     refuses a row that points into another practice

-- ---------------------------------------------------------------------------
-- Tenancy
-- ---------------------------------------------------------------------------

CREATE TABLE tenants (
  id              BINARY(16)    NOT NULL,
  slug            VARCHAR(63)   NOT NULL,
  name            VARCHAR(255)  NOT NULL,
  status          ENUM('active','suspended','offboarding') NOT NULL DEFAULT 'active',
  timezone        VARCHAR(64)   NOT NULL DEFAULT 'Africa/Johannesburg',
  practice_number VARCHAR(32)   NULL,
  address         VARCHAR(500)  NULL,
  phone           VARCHAR(32)   NULL,
  email           VARCHAR(255)  NULL,
  settings_json   JSON          NULL,
  created_at      DATETIME(3)   NOT NULL,
  updated_at      DATETIME(3)   NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_tenants_slug (slug)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Patient record (created before users: a patient login points at a patient)
-- ---------------------------------------------------------------------------

CREATE TABLE patients (
  id                     BINARY(16)    NOT NULL,
  tenant_id              BINARY(16)    NOT NULL,
  first_name             VARCHAR(100)  NOT NULL,
  last_name              VARCHAR(100)  NOT NULL,
  date_of_birth          DATE          NOT NULL,
  sex                    ENUM('female','male','intersex','unknown') NOT NULL,
  id_type                ENUM('sa_id','passport','other') NULL,
  national_id            VARCHAR(32)   NULL,
  mobile                 VARCHAR(20)   NULL,
  email                  VARCHAR(255)  NULL,
  address                VARCHAR(500)  NULL,
  preferred_language     VARCHAR(32)   NULL,
  preferred_channel      ENUM('whatsapp','sms','push','email','none') NOT NULL DEFAULT 'none',
  waking_start           TIME          NULL,
  waking_end             TIME          NULL,
  review_interval_months TINYINT UNSIGNED NULL,
  medical_aid_name       VARCHAR(100)  NULL,
  medical_aid_number     VARCHAR(50)   NULL,
  nok_name               VARCHAR(200)  NULL,
  nok_relationship       VARCHAR(50)   NULL,
  nok_phone              VARCHAR(20)   NULL,
  smoking_status         ENUM('never','former','current','unknown') NOT NULL DEFAULT 'unknown',
  alcohol_use            ENUM('none','occasional','regular','heavy','unknown') NOT NULL DEFAULT 'unknown',
  past_surgical_history  TEXT          NULL,
  family_history         TEXT          NULL,
  status                 ENUM('active','inactive','deceased') NOT NULL DEFAULT 'active',
  created_by             BINARY(16)    NULL,
  created_at             DATETIME(3)   NOT NULL,
  updated_at             DATETIME(3)   NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_patients_tenant_id (tenant_id, id),
  -- The same person at two practices is two records, so uniqueness is per
  -- tenant. NULLs do not collide, so patients without an ID number are fine.
  UNIQUE KEY uq_patients_national_id (tenant_id, national_id),
  KEY idx_patients_name (tenant_id, last_name, first_name),
  KEY idx_patients_dob (tenant_id, date_of_birth),
  KEY idx_patients_mobile (tenant_id, mobile),
  CONSTRAINT fk_patients_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Identity
-- ---------------------------------------------------------------------------

CREATE TABLE users (
  id                   BINARY(16)      NOT NULL,
  tenant_id            BINARY(16)      NOT NULL,
  email                VARCHAR(255)    NOT NULL,
  full_name            VARCHAR(200)    NOT NULL,
  mobile               VARCHAR(20)     NULL,
  password_hash        VARCHAR(255)    NOT NULL,
  role                 ENUM('super_admin','clinic_admin','doctor','nurse','patient','caregiver') NOT NULL,
  professional_reg_no  VARCHAR(64)     NULL,
  patient_id           BINARY(16)      NULL,
  status               ENUM('pending','active','suspended','deactivated') NOT NULL DEFAULT 'pending',
  mfa_secret           VARBINARY(255)  NULL,
  mfa_enabled          TINYINT(1)      NOT NULL DEFAULT 0,
  mfa_last_counter     BIGINT UNSIGNED NULL,
  mfa_enrolled_at      DATETIME(3)     NULL,
  failed_attempts      INT UNSIGNED    NOT NULL DEFAULT 0,
  locked_until         DATETIME(3)     NULL,
  last_login_at        DATETIME(3)     NULL,
  password_changed_at  DATETIME(3)     NULL,
  created_by           BINARY(16)      NULL,
  created_at           DATETIME(3)     NOT NULL,
  updated_at           DATETIME(3)     NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_users_tenant_id (tenant_id, id),
  UNIQUE KEY uq_users_email (tenant_id, email),
  -- One login per patient record.
  UNIQUE KEY uq_users_patient (tenant_id, patient_id),
  CONSTRAINT fk_users_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id),
  CONSTRAINT fk_users_patient FOREIGN KEY (tenant_id, patient_id) REFERENCES patients (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Ten single-use codes per user, stored as SHA-256 (80 bits of randomness
-- each, so a slow hash would add cost without adding safety).
CREATE TABLE user_recovery_codes (
  id          BINARY(16)   NOT NULL,
  tenant_id   BINARY(16)   NOT NULL,
  user_id     BINARY(16)   NOT NULL,
  code_hash   BINARY(32)   NOT NULL,
  created_at  DATETIME(3)  NOT NULL,
  used_at     DATETIME(3)  NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_recovery_codes_hash (tenant_id, user_id, code_hash),
  CONSTRAINT fk_recovery_codes_user FOREIGN KEY (tenant_id, user_id) REFERENCES users (tenant_id, id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Opaque, revocable sessions. Only the SHA-256 of the bearer token is kept.
CREATE TABLE sessions (
  id               BINARY(16)    NOT NULL,
  tenant_id        BINARY(16)    NOT NULL,
  user_id          BINARY(16)    NOT NULL,
  token_hash       BINARY(32)    NOT NULL,
  mfa_pending      TINYINT(1)    NOT NULL DEFAULT 0,
  issued_at        DATETIME(3)   NOT NULL,
  last_seen_at     DATETIME(3)   NOT NULL,
  absolute_expiry  DATETIME(3)   NOT NULL,
  revoked_at       DATETIME(3)   NULL,
  revoked_reason   VARCHAR(64)   NULL,
  ip               VARCHAR(45)   NULL,
  user_agent       VARCHAR(255)  NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_sessions_token (token_hash),
  KEY idx_sessions_user (tenant_id, user_id, revoked_at),
  KEY idx_sessions_expiry (absolute_expiry),
  CONSTRAINT fk_sessions_user FOREIGN KEY (tenant_id, user_id) REFERENCES users (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Clinical baseline
-- ---------------------------------------------------------------------------

CREATE TABLE patient_allergies (
  id           BINARY(16)    NOT NULL,
  tenant_id    BINARY(16)    NOT NULL,
  patient_id   BINARY(16)    NOT NULL,
  substance    VARCHAR(200)  NOT NULL,
  reaction     VARCHAR(500)  NULL,
  severity     ENUM('mild','moderate','severe','life_threatening','unknown') NOT NULL DEFAULT 'unknown',
  status       ENUM('active','resolved','entered_in_error') NOT NULL DEFAULT 'active',
  recorded_by  BINARY(16)    NOT NULL,
  recorded_at  DATETIME(3)   NOT NULL,
  updated_by   BINARY(16)    NULL,
  updated_at   DATETIME(3)   NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_allergies_tenant_id (tenant_id, id),
  KEY idx_allergies_patient (tenant_id, patient_id, status),
  CONSTRAINT fk_allergies_patient FOREIGN KEY (tenant_id, patient_id) REFERENCES patients (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE patient_conditions (
  id             BINARY(16)    NOT NULL,
  tenant_id      BINARY(16)    NOT NULL,
  patient_id     BINARY(16)    NOT NULL,
  icd10_code     VARCHAR(10)   NULL,
  description    VARCHAR(255)  NOT NULL,
  is_chronic     TINYINT(1)    NOT NULL DEFAULT 0,
  status         ENUM('active','resolved','entered_in_error') NOT NULL DEFAULT 'active',
  onset_date     DATE          NULL,
  resolved_date  DATE          NULL,
  recorded_by    BINARY(16)    NOT NULL,
  recorded_at    DATETIME(3)   NOT NULL,
  updated_by     BINARY(16)    NULL,
  updated_at     DATETIME(3)   NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_conditions_tenant_id (tenant_id, id),
  KEY idx_conditions_patient (tenant_id, patient_id, status),
  KEY idx_conditions_chronic (tenant_id, is_chronic, status, icd10_code),
  CONSTRAINT fk_conditions_patient FOREIGN KEY (tenant_id, patient_id) REFERENCES patients (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Append-only: one row per decision per purpose. Withdrawal is a new row.
-- seq gives a total order even when two decisions share a millisecond.
CREATE TABLE patient_consents (
  id           BINARY(16)       NOT NULL,
  seq          BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  tenant_id    BINARY(16)       NOT NULL,
  patient_id   BINARY(16)       NOT NULL,
  purpose      ENUM('data_processing','ai_assisted_review','reminder_communications') NOT NULL,
  granted      TINYINT(1)       NOT NULL,
  method       ENUM('written','verbal','electronic') NOT NULL,
  note         VARCHAR(500)     NULL,
  recorded_by  BINARY(16)       NOT NULL,
  recorded_at  DATETIME(3)      NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_consents_seq (seq),
  UNIQUE KEY uq_consents_tenant_id (tenant_id, id),
  KEY idx_consents_patient (tenant_id, patient_id, purpose, seq),
  CONSTRAINT fk_consents_patient FOREIGN KEY (tenant_id, patient_id) REFERENCES patients (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- ---------------------------------------------------------------------------
-- Audit (BR-8.1, BR-8.2). tenant_id is nullable for platform events such as a
-- login attempt against a practice that does not exist.
--
-- Append-only by convention in code. In production enforce it with grants:
--   GRANT SELECT, INSERT ON mes.audit_log TO 'mes_app'@'%';
-- ---------------------------------------------------------------------------

CREATE TABLE audit_log (
  id            BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  tenant_id     BINARY(16)       NULL,
  actor_id      BINARY(16)       NULL,
  actor_role    VARCHAR(32)      NULL,
  action        VARCHAR(64)      NOT NULL,
  subject_type  VARCHAR(32)      NULL,
  subject_id    BINARY(16)       NULL,
  outcome       ENUM('success','failure','denied') NOT NULL,
  reason        VARCHAR(255)     NULL,
  request_id    VARCHAR(64)      NULL,
  ip            VARCHAR(45)      NULL,
  metadata      JSON             NULL,
  occurred_at   DATETIME(3)      NOT NULL,
  PRIMARY KEY (id),
  KEY idx_audit_tenant_time (tenant_id, occurred_at),
  KEY idx_audit_subject (tenant_id, subject_type, subject_id),
  KEY idx_audit_actor (tenant_id, actor_id, occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
