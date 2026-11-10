-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- Revocation is an immutable API fact, independent of controller ownership.
-- Older binaries may continue writing their epoch during the expand window.
ALTER TABLE control.grant_revocation ALTER COLUMN owner_epoch DROP NOT NULL;
