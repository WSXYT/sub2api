-- Relay station traffic has no local upstream Account, but it must retain
-- normal user/API-key/group billing and usage history.
ALTER TABLE usage_logs
    ALTER COLUMN account_id DROP NOT NULL;
