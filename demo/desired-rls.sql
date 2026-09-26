CREATE TABLE demo_rls (
    id bigint PRIMARY KEY,
    owner_id bigint NOT NULL
);
ALTER TABLE demo_rls ENABLE ROW LEVEL SECURITY;
CREATE POLICY readers ON demo_rls FOR SELECT USING (owner_id = 7);
