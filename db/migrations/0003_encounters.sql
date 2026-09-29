-- 0003_encounters.sql — consultation notes (BR-2.4, BR-2.7) and AI drafts.

-- A consultation note. Draft → signed, one way. Once signed the body fields
-- change only through encounter_amendments, which keep what they said before.
CREATE TABLE encounters (
  id                    BINARY(16)    NOT NULL,
  tenant_id             BINARY(16)    NOT NULL,
  patient_id            BINARY(16)    NOT NULL,
  clinician_id          BINARY(16)    NOT NULL,
  encounter_type        ENUM('consultation','follow_up','telephonic','home_visit','procedure','other')
                        NOT NULL DEFAULT 'consultation',
  encounter_at          DATETIME(3)   NOT NULL,
  presenting_complaint  TEXT          NULL,
  history               TEXT          NULL,
  examination           TEXT          NULL,
  assessment            TEXT          NULL,
  plan                  TEXT          NULL,
  vitals_json           JSON          NULL,
  status                ENUM('draft','signed') NOT NULL DEFAULT 'draft',
  signed_at             DATETIME(3)   NULL,
  signed_by             BINARY(16)    NULL,
  -- Provenance, permanent: set once a model has drafted text for this note.
  ai_assisted           TINYINT(1)    NOT NULL DEFAULT 0,
  amended_at            DATETIME(3)   NULL,
  created_at            DATETIME(3)   NOT NULL,
  updated_at            DATETIME(3)   NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_encounters_tenant_id (tenant_id, id),
  KEY idx_encounters_patient (tenant_id, patient_id, status, encounter_at),
  KEY idx_encounters_clinician (tenant_id, clinician_id, status),
  CONSTRAINT fk_encounters_patient FOREIGN KEY (tenant_id, patient_id) REFERENCES patients (tenant_id, id),
  CONSTRAINT fk_encounters_clinician FOREIGN KEY (tenant_id, clinician_id) REFERENCES users (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- One row per field changed after signing. The encounter row carries the
-- current text; the original is reconstructed by walking these backwards.
CREATE TABLE encounter_amendments (
  id              BINARY(16)       NOT NULL,
  seq             BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  tenant_id       BINARY(16)       NOT NULL,
  encounter_id    BINARY(16)       NOT NULL,
  field           ENUM('presenting_complaint','history','examination','assessment','plan') NOT NULL,
  previous_value  TEXT             NULL,
  new_value       TEXT             NULL,
  reason          VARCHAR(500)     NOT NULL,
  amended_by      BINARY(16)       NOT NULL,
  amended_at      DATETIME(3)      NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_amendments_seq (seq),
  KEY idx_amendments_encounter (tenant_id, encounter_id, seq),
  CONSTRAINT fk_amendments_encounter FOREIGN KEY (tenant_id, encounter_id) REFERENCES encounters (tenant_id, id),
  CONSTRAINT fk_amendments_user FOREIGN KEY (tenant_id, amended_by) REFERENCES users (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- AI drafts, stored BESIDE the record and never inside it. Both the
-- clinician's dictation and what the model made of it are kept, so a
-- disputed note can be traced (BR-5.12), and a clinician can flag a draft
-- as wrong or unsafe for review (BR-5.15).
CREATE TABLE encounter_note_drafts (
  id             BINARY(16)     NOT NULL,
  tenant_id      BINARY(16)     NOT NULL,
  encounter_id   BINARY(16)     NOT NULL,
  dictation      MEDIUMTEXT     NOT NULL,
  context_json   JSON           NULL,
  draft_json     JSON           NULL,
  model          VARCHAR(100)   NOT NULL,
  status         ENUM('generated','failed') NOT NULL,
  error          VARCHAR(500)   NULL,
  input_tokens   INT UNSIGNED   NULL,
  output_tokens  INT UNSIGNED   NULL,
  created_by     BINARY(16)     NOT NULL,
  created_at     DATETIME(3)    NOT NULL,
  flagged_at     DATETIME(3)    NULL,
  flagged_by     BINARY(16)     NULL,
  flag_reason    VARCHAR(1000)  NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_drafts_tenant_id (tenant_id, id),
  KEY idx_drafts_encounter (tenant_id, encounter_id, created_at),
  KEY idx_drafts_usage (tenant_id, created_at),
  CONSTRAINT fk_drafts_encounter FOREIGN KEY (tenant_id, encounter_id) REFERENCES encounters (tenant_id, id),
  CONSTRAINT fk_drafts_user FOREIGN KEY (tenant_id, created_by) REFERENCES users (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
