-- 0002_medication.sql — medication catalogue, regimens and dose records.

-- ---------------------------------------------------------------------------
-- The medication catalogue is shared reference data that every practice
-- reads (BR-0.10). It is deliberately NOT tenant-scoped: what belongs to a
-- practice is what it prescribes and stocks, not the list of medicines that
-- exist. brand_name is '' rather than NULL for generic entries so the unique
-- key below also catches duplicate generics.
-- ---------------------------------------------------------------------------

CREATE TABLE medications (
  id            BINARY(16)    NOT NULL,
  generic_name  VARCHAR(200)  NOT NULL,
  brand_name    VARCHAR(200)  NOT NULL DEFAULT '',
  strength      VARCHAR(100)  NOT NULL,
  dosage_form   VARCHAR(50)   NOT NULL,
  route         ENUM('oral','sublingual','topical','inhaled','subcutaneous','intramuscular','intravenous',
                     'rectal','vaginal','ophthalmic','otic','nasal','transdermal','other') NOT NULL DEFAULT 'oral',
  schedule      VARCHAR(4)    NULL,
  nappi_code    VARCHAR(20)   NULL,
  pack_size     VARCHAR(50)   NULL,
  is_active     TINYINT(1)    NOT NULL DEFAULT 1,
  created_at    DATETIME(3)   NOT NULL,
  updated_at    DATETIME(3)   NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_medications_nappi (nappi_code),
  UNIQUE KEY uq_medications_product (generic_name, brand_name, strength, dosage_form),
  KEY idx_medications_brand (brand_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- A starter set of common chronic medicines so a new installation can
-- prescribe at once. Scheduling status and NAPPI codes are deliberately left
-- empty: load the authoritative list with `go run ./cmd/catalogue -import`
-- rather than rely on values typed into a migration.
INSERT INTO medications (id, generic_name, strength, dosage_form, route, created_at, updated_at) VALUES
  (UNHEX(REPLACE(UUID(), '-', '')), 'Metformin', '500 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Metformin', '850 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Gliclazide', '80 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Glimepiride', '2 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Insulin, biphasic human (30/70)', '100 IU/ml', 'injection', 'subcutaneous', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Amlodipine', '5 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Amlodipine', '10 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Enalapril', '10 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Perindopril', '4 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Hydrochlorothiazide', '12.5 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Hydrochlorothiazide', '25 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Furosemide', '40 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Spironolactone', '25 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Carvedilol', '6.25 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Atorvastatin', '20 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Simvastatin', '20 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Aspirin', '81 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Warfarin', '5 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Tenofovir/lamivudine/dolutegravir', '300/300/50 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Tenofovir/emtricitabine/efavirenz', '300/200/600 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Isoniazid', '300 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Salbutamol', '100 mcg/dose', 'inhaler', 'inhaled', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Budesonide', '200 mcg/dose', 'inhaler', 'inhaled', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Levothyroxine', '50 mcg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Levothyroxine', '100 mcg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Carbamazepine', '200 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Sodium valproate', '200 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Allopurinol', '100 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Omeprazole', '20 mg', 'capsule', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Fluoxetine', '20 mg', 'capsule', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Amitriptyline', '25 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)),
  (UNHEX(REPLACE(UUID(), '-', '')), 'Paracetamol', '500 mg', 'tablet', 'oral', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3));

-- ---------------------------------------------------------------------------
-- Regimens (BR-4.1, BR-4.10): what a patient has been told to take.
-- ---------------------------------------------------------------------------

CREATE TABLE regimens (
  id                 BINARY(16)    NOT NULL,
  tenant_id          BINARY(16)    NOT NULL,
  patient_id         BINARY(16)    NOT NULL,
  medication_id      BINARY(16)    NOT NULL,
  dose               VARCHAR(100)  NOT NULL,
  route              ENUM('oral','sublingual','topical','inhaled','subcutaneous','intramuscular','intravenous',
                          'rectal','vaginal','ophthalmic','otic','nasal','transdermal','other') NOT NULL,
  frequency          ENUM('once_daily','twice_daily','three_times_daily','four_times_daily',
                          'every_other_day','weekly','as_needed') NOT NULL,
  instructions       VARCHAR(500)  NULL,
  start_date         DATE          NOT NULL,
  end_date           DATE          NULL,
  status             ENUM('active','paused','stopped','completed') NOT NULL DEFAULT 'active',
  status_reason      VARCHAR(255)  NULL,
  status_changed_by  BINARY(16)    NULL,
  status_changed_at  DATETIME(3)   NULL,
  prescribed_by      BINARY(16)    NOT NULL,
  created_at         DATETIME(3)   NOT NULL,
  updated_at         DATETIME(3)   NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_regimens_tenant_id (tenant_id, id),
  -- Lets dose_events prove, in the database, that a dose belongs to the same
  -- patient as its regimen.
  UNIQUE KEY uq_regimens_patient_link (tenant_id, id, patient_id),
  KEY idx_regimens_patient (tenant_id, patient_id, status),
  CONSTRAINT fk_regimens_patient FOREIGN KEY (tenant_id, patient_id) REFERENCES patients (tenant_id, id),
  CONSTRAINT fk_regimens_medication FOREIGN KEY (medication_id) REFERENCES medications (id),
  CONSTRAINT fk_regimens_prescriber FOREIGN KEY (tenant_id, prescribed_by) REFERENCES users (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- One row per scheduled dose.
--   pending  the time has not passed, or the response window is still open.
--            NOT a failure: merging it with missed would count every future
--            dose against the patient.
--   taken / skipped  the patient (or staff for them) answered
--   missed   the response window closed with no answer
CREATE TABLE dose_events (
  id                BINARY(16)    NOT NULL,
  tenant_id         BINARY(16)    NOT NULL,
  patient_id        BINARY(16)    NOT NULL,
  regimen_id        BINARY(16)    NOT NULL,
  scheduled_at      DATETIME(3)   NOT NULL,
  status            ENUM('pending','taken','skipped','missed') NOT NULL DEFAULT 'pending',
  responded_at      DATETIME(3)   NULL,
  -- How the answer arrived: a patient's own reply is different evidence from
  -- a staff member ticking a box.
  response_channel  ENUM('whatsapp','sms','push','portal','staff') NULL,
  responded_by      BINARY(16)    NULL,
  skip_reason       VARCHAR(255)  NULL,
  created_at        DATETIME(3)   NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_doses_tenant_id (tenant_id, id),
  UNIQUE KEY uq_doses_slot (tenant_id, regimen_id, scheduled_at),
  KEY idx_doses_patient (tenant_id, patient_id, scheduled_at),
  KEY idx_doses_status (tenant_id, status, scheduled_at),
  CONSTRAINT fk_doses_regimen FOREIGN KEY (tenant_id, regimen_id, patient_id)
    REFERENCES regimens (tenant_id, id, patient_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
