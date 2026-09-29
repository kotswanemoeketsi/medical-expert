-- 0005_labs_documents.sql — laboratory results, referral letters and medical
-- certificates.

-- A laboratory report, captured by hand. has_abnormal and has_critical
-- summarise the flags the laboratory printed; they are never derived from
-- the values here.
CREATE TABLE lab_results (
  id                     BINARY(16)     NOT NULL,
  tenant_id              BINARY(16)     NOT NULL,
  patient_id             BINARY(16)     NOT NULL,
  lab_name               VARCHAR(100)   NOT NULL,
  lab_reference          VARCHAR(100)   NULL,
  panel                  VARCHAR(200)   NOT NULL,
  specimen_collected_at  DATETIME(3)    NULL,
  reported_at            DATETIME(3)    NOT NULL,
  status                 ENUM('preliminary','final','corrected') NOT NULL DEFAULT 'final',
  has_abnormal           TINYINT(1)     NOT NULL DEFAULT 0,
  has_critical           TINYINT(1)     NOT NULL DEFAULT 0,
  comment                TEXT           NULL,
  recorded_by            BINARY(16)     NOT NULL,
  recorded_at            DATETIME(3)    NOT NULL,
  -- A result nobody looked at is how a practice harms someone while doing
  -- everything else correctly.
  acknowledged_at        DATETIME(3)    NULL,
  acknowledged_by        BINARY(16)     NULL,
  acknowledgement_note   VARCHAR(1000)  NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_lab_results_tenant_id (tenant_id, id),
  KEY idx_lab_results_patient (tenant_id, patient_id, reported_at),
  KEY idx_lab_results_inbox (tenant_id, acknowledged_at, has_critical, has_abnormal, reported_at),
  CONSTRAINT fk_lab_results_patient FOREIGN KEY (tenant_id, patient_id) REFERENCES patients (tenant_id, id),
  CONSTRAINT fk_lab_results_recorder FOREIGN KEY (tenant_id, recorded_by) REFERENCES users (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- One row per analyte. Value, unit, reference range and flag are stored
-- exactly as the laboratory reported them: ranges differ between
-- laboratories, assays and populations, and a flag computed here would
-- sometimes contradict the printed report in the patient's file.
CREATE TABLE lab_result_items (
  id               BINARY(16)         NOT NULL,
  tenant_id        BINARY(16)         NOT NULL,
  result_id        BINARY(16)         NOT NULL,
  line_no          SMALLINT UNSIGNED  NOT NULL,
  analyte          VARCHAR(100)       NOT NULL,
  value            VARCHAR(50)        NOT NULL,
  unit             VARCHAR(30)        NULL,
  reference_range  VARCHAR(60)        NULL,
  abnormal_flag    ENUM('none','low','high','abnormal','critical_low','critical_high','critical')
                   NOT NULL DEFAULT 'none',
  PRIMARY KEY (id),
  UNIQUE KEY uq_lab_result_items_line (tenant_id, result_id, line_no),
  CONSTRAINT fk_lab_result_items_result FOREIGN KEY (tenant_id, result_id) REFERENCES lab_results (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Referral letters and medical certificates. Both leave the practice and are
-- relied on by someone outside it, so rendered_text is produced once, at
-- issue, and never regenerated.
CREATE TABLE clinical_documents (
  id                   BINARY(16)    NOT NULL,
  tenant_id            BINARY(16)    NOT NULL,
  patient_id           BINARY(16)    NOT NULL,
  doc_type             ENUM('referral','sick_note') NOT NULL,
  status               ENUM('draft','issued','cancelled') NOT NULL DEFAULT 'draft',
  document_number      VARCHAR(20)   NULL,
  author_id            BINARY(16)    NOT NULL,
  encounter_id         BINARY(16)    NULL,
  body_json            JSON          NOT NULL,
  rendered_text        MEDIUMTEXT    NULL,
  basis                ENUM('examination','patient_report') NULL,
  examined_on          DATE          NULL,
  valid_from           DATE          NULL,
  valid_to             DATE          NULL,
  discloses_diagnosis  TINYINT(1)    NOT NULL DEFAULT 0,
  backdate_reason      VARCHAR(500)  NULL,
  issued_at            DATETIME(3)   NULL,
  issued_by            BINARY(16)    NULL,
  cancelled_at         DATETIME(3)   NULL,
  cancelled_by         BINARY(16)    NULL,
  cancel_reason        VARCHAR(255)  NULL,
  created_at           DATETIME(3)   NOT NULL,
  updated_at           DATETIME(3)   NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_documents_tenant_id (tenant_id, id),
  UNIQUE KEY uq_documents_number (tenant_id, document_number),
  KEY idx_documents_patient (tenant_id, patient_id, created_at),
  CONSTRAINT fk_documents_patient FOREIGN KEY (tenant_id, patient_id) REFERENCES patients (tenant_id, id),
  CONSTRAINT fk_documents_author FOREIGN KEY (tenant_id, author_id) REFERENCES users (tenant_id, id),
  CONSTRAINT fk_documents_encounter FOREIGN KEY (tenant_id, encounter_id) REFERENCES encounters (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Human-quotable numbers per practice, per type, per year: REF-2026-0001,
-- MC-2026-0007. The row lock on this table serialises numbering.
CREATE TABLE document_sequences (
  tenant_id    BINARY(16)         NOT NULL,
  doc_type     ENUM('referral','sick_note') NOT NULL,
  year         SMALLINT UNSIGNED  NOT NULL,
  last_number  INT UNSIGNED       NOT NULL,
  PRIMARY KEY (tenant_id, doc_type, year),
  CONSTRAINT fk_document_sequences_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
