-- Creates the databases and the application account for local development.
--
-- Change the password below first, then run as an administrative user:
--   mysql -u root -p < db/setup.sql                 (Linux, macOS, Windows cmd.exe)
--   Get-Content db\setup.sql | mysql -u root -p      (Windows PowerShell)
-- or open the file in MySQL Workbench / HeidiSQL and execute it.
--
-- The schema itself is created by `go run ./cmd/migrate`, not by this file.
--
-- Production: use two accounts. A migration account with DDL rights that is
-- used only by the deployment step, and an application account with
--   SELECT, INSERT, UPDATE, DELETE ON mes.*
-- except the audit log, which should be append-only at the database level:
--   REVOKE UPDATE, DELETE ON mes.audit_log FROM 'mes_app'@'%';
-- (grant table-level rights instead of mes.* to make that stick).

CREATE DATABASE IF NOT EXISTS mes      CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS mes_test CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

-- Both host forms, because a TCP connection to 127.0.0.1 matches one or the
-- other depending on server configuration.
CREATE USER IF NOT EXISTS 'mes'@'localhost' IDENTIFIED BY 'change-this-password';
CREATE USER IF NOT EXISTS 'mes'@'127.0.0.1' IDENTIFIED BY 'change-this-password';

GRANT ALL PRIVILEGES ON mes.*      TO 'mes'@'localhost';
GRANT ALL PRIVILEGES ON mes.*      TO 'mes'@'127.0.0.1';
GRANT ALL PRIVILEGES ON mes_test.* TO 'mes'@'localhost';
GRANT ALL PRIVILEGES ON mes_test.* TO 'mes'@'127.0.0.1';

FLUSH PRIVILEGES;
