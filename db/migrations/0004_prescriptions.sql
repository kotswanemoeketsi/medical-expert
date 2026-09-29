-- 0004_prescriptions.sql — repeat prescriptions for chronic patients.

-- An authorisation, or a repeat issued against one. A repeat carries
-- repeat_of_id pointing at the original, which holds repeats_authorised and
-- repeats_used — two columns rather than a countdown, so the original
-- authorisation stays visible after the repeats are spent.
CREATE TABLE prescriptions (
  id                  BINARY(16)        NOT NULL,
  tenant_id           BINARY(16)        NOT NULL,
  patient_id          BINARY(16)        NOT NULL,
  prescriber_id       BINARY(16)        NOT NULL,
  repeat_of_id        BINARY(16)        NULL,
  issued_at           DATETIME(3)       NOT NULL,
  valid_until         DATE              NOT NULL,
  repeats_authorised  TINYINT UNSIGNED  NOT NULL DEFAULT 0,
  repeats_used        TINYINT UNSIGNED  NOT NULL DEFAULT 0,
  status              ENUM('active','cancelled','expired') NOT NULL DEFAULT 'active',
  -- Required when the patient is overdue for review; recorded on the script.
  override_reason     VARCHAR(500)      NULL,
  notes               VARCHAR(1000)     NULL,
  cancel_reason       VARCHAR(255)      NULL,
  cancelled_by        BINARY(16)        NULL,
  cancelled_at        DATETIME(3)       NULL,
  created_at          DATETIME(3)       NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_prescriptions_tenant_id (tenant_id, id),
  KEY idx_prescriptions_patient (tenant_id, patient_id, issued_at),
  KEY idx_prescriptions_status (tenant_id, status, valid_until),
  KEY idx_prescriptions_repeat_of (tenant_id, repeat_of_id),
  CONSTRAINT fk_prescriptions_patient FOREIGN KEY (tenant_id, patient_id) REFERENCES patients (tenant_id, id),
  CONSTRAINT fk_prescriptions_prescriber FOREIGN KEY (tenant_id, prescriber_id) REFERENCES users (tenant_id, id),
  CONSTRAINT fk_prescriptions_repeat_of FOREIGN KEY (tenant_id, repeat_of_id) REFERENCES prescriptions (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Items are COPIED onto the script, not referenced live from the regimen:
-- if the regimen changes next month, last month's script must not change
-- with it. regimen_id and medication_id are provenance only.
CREATE TABLE prescription_items (
  id               BINARY(16)        NOT NULL,
  tenant_id        BINARY(16)        NOT NULL,
  prescription_id  BINARY(16)        NOT NULL,
  line_no          TINYINT UNSIGNED  NOT NULL,
  regimen_id       BINARY(16)        NULL,
  medication_id    BINARY(16)        NULL,
  medication_name  VARCHAR(400)      NOT NULL,
  dose             VARCHAR(100)      NOT NULL,
  route            VARCHAR(30)       NOT NULL,
  frequency        VARCHAR(60)       NOT NULL,
  quantity         VARCHAR(50)       NOT NULL,
  instructions     VARCHAR(500)      NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_prescription_items_line (tenant_id, prescription_id, line_no),
  CONSTRAINT fk_prescription_items_prescription FOREIGN KEY (tenant_id, prescription_id)
    REFERENCES prescriptions (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
