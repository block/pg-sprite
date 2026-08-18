-- Deliberately risky DDL for the offline commands (lint, suggest): a
-- blocking index build, a rewrite-forcing type change, a destructive drop,
-- a blocking NOT NULL flip (all warnings), and an operation the engine
-- refuses outright (an error — this is what makes lint exit non-zero).
CREATE INDEX idx_users_email ON users (email);
ALTER TABLE orders ALTER COLUMN user_id TYPE bigint;
ALTER TABLE users DROP COLUMN status;
ALTER TABLE users ALTER COLUMN email SET NOT NULL;
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
