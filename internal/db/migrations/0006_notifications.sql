-- 0006_notifications.sql — WhatsApp reminders and recalls, patients' replies,
-- and what the dose scheduler and the reminder dispatcher need.

-- source: 'schedule' doses are generated from the regimen and kept in step
-- with it; 'manual' doses were added by hand and the scheduler never touches
-- them. Every dose from stage 2 was added by hand.
--
-- reminded_at and followed_up_at are set when the dispatcher claims a dose,
-- in the same transaction that writes the notification, so two dispatchers
-- can never both remind the same dose.
ALTER TABLE dose_events
  ADD COLUMN source         ENUM('schedule','manual') NOT NULL DEFAULT 'manual' AFTER scheduled_at,
  ADD COLUMN reminded_at    DATETIME(3) NULL AFTER source,
  ADD COLUMN followed_up_at DATETIME(3) NULL AFTER reminded_at;

-- A patient can withdraw consent to reminders by replying STOP, and there is
-- then no staff member to name. NULL means the patient did it themselves.
ALTER TABLE patient_consents MODIFY recorded_by BINARY(16) NULL;

-- One outgoing message. The row is written BEFORE the send, so a message that
-- fails mid-flight is still visible: silence would be indistinguishable from
-- a patient ignoring the reminder.
CREATE TABLE notifications (
  id                   BINARY(16)     NOT NULL,
  tenant_id            BINARY(16)     NOT NULL,
  patient_id           BINARY(16)     NOT NULL,
  channel              ENUM('whatsapp') NOT NULL,
  purpose              ENUM('dose_reminder','dose_follow_up','recall') NOT NULL,
  -- The number as it was when the message was sent; the patient's record
  -- may change later.
  destination          VARCHAR(20)    NOT NULL,
  template_name        VARCHAR(100)   NOT NULL,
  template_language    VARCHAR(10)    NOT NULL,
  params_json          JSON           NOT NULL,
  -- The message as the patient sees it, for staff.
  preview              VARCHAR(1000)  NOT NULL,
  -- Opaque value carried by the reply buttons, so a button press finds its
  -- reminder without trusting anything else in the reply.
  reply_token          CHAR(22)       NULL,
  status               ENUM('queued','sent','delivered','read','failed') NOT NULL DEFAULT 'queued',
  attempts             TINYINT UNSIGNED NOT NULL DEFAULT 0,
  -- While queued: when the next attempt may start. A sender leases the row
  -- by moving this forward before it sends.
  next_attempt_at      DATETIME(3)    NULL,
  provider_message_id  VARCHAR(128)   NULL,
  error_code           VARCHAR(32)    NULL,
  error_detail         VARCHAR(500)   NULL,
  -- The staff member who asked for it; NULL for the scheduler.
  created_by           BINARY(16)     NULL,
  created_at           DATETIME(3)    NOT NULL,
  sent_at              DATETIME(3)    NULL,
  delivered_at         DATETIME(3)    NULL,
  read_at              DATETIME(3)    NULL,
  failed_at            DATETIME(3)    NULL,
  updated_at           DATETIME(3)    NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_notifications_tenant_id (tenant_id, id),
  UNIQUE KEY uq_notifications_provider_id (provider_message_id),
  UNIQUE KEY uq_notifications_reply_token (reply_token),
  KEY idx_notifications_patient (tenant_id, patient_id, created_at),
  KEY idx_notifications_queue (tenant_id, status, next_attempt_at),
  KEY idx_notifications_created (tenant_id, created_at),
  -- Finds the latest reminder to a number when a reply carries nothing else.
  KEY idx_notifications_destination (destination, created_at),
  CONSTRAINT fk_notifications_patient FOREIGN KEY (tenant_id, patient_id) REFERENCES patients (tenant_id, id),
  CONSTRAINT fk_notifications_creator FOREIGN KEY (tenant_id, created_by) REFERENCES users (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- The doses a reminder was about. One reminder covers every dose a patient
-- has at the same time, so a single reply answers them together.
CREATE TABLE notification_doses (
  tenant_id        BINARY(16)  NOT NULL,
  notification_id  BINARY(16)  NOT NULL,
  dose_event_id    BINARY(16)  NOT NULL,
  PRIMARY KEY (tenant_id, notification_id, dose_event_id),
  KEY idx_notification_doses_dose (tenant_id, dose_event_id),
  CONSTRAINT fk_notification_doses_notification FOREIGN KEY (tenant_id, notification_id)
    REFERENCES notifications (tenant_id, id),
  CONSTRAINT fk_notification_doses_dose FOREIGN KEY (tenant_id, dose_event_id)
    REFERENCES dose_events (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- A patient's reply, kept whether or not it could be read, so staff can see
-- a message such as "I ran out of pills" rather than it vanishing.
CREATE TABLE notification_replies (
  id                   BINARY(16)     NOT NULL,
  tenant_id            BINARY(16)     NOT NULL,
  patient_id           BINARY(16)     NOT NULL,
  notification_id      BINARY(16)     NULL,
  -- Unique, so a webhook delivered twice is processed once.
  provider_message_id  VARCHAR(128)   NOT NULL,
  received_at          DATETIME(3)    NOT NULL,
  interpretation       ENUM('taken','skipped','stop','unrecognised') NOT NULL,
  body                 VARCHAR(1000)  NULL,
  doses_updated        SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  created_at           DATETIME(3)    NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_notification_replies_tenant_id (tenant_id, id),
  UNIQUE KEY uq_notification_replies_provider_id (provider_message_id),
  KEY idx_notification_replies_patient (tenant_id, patient_id, received_at),
  KEY idx_notification_replies_received (tenant_id, received_at),
  CONSTRAINT fk_notification_replies_patient FOREIGN KEY (tenant_id, patient_id) REFERENCES patients (tenant_id, id),
  CONSTRAINT fk_notification_replies_notification FOREIGN KEY (tenant_id, notification_id)
    REFERENCES notifications (tenant_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
