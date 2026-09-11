#!/usr/bin/env python3
"""Exercise the localhost-only compose/supabase.yml services fixture."""
import base64
import hashlib
import hmac
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
COMPOSE = ["docker", "compose", "-p", "pgsprite-supabase", "-f", str(ROOT / "compose/supabase.yml")]
BINARY = ROOT / "bin/pg-sprite"
TABLE = "public.pgsprite_service_probe"
SECRET = b"pgsprite-local-test-jwt-secret-not-for-production"


def sql(statement):
    result = subprocess.run(COMPOSE + ["exec", "-T", "postgres", "psql", "-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-At"], input=statement, text=True, capture_output=True, timeout=15, check=True)
    return result.stdout.strip()


def change(port, statement, transaction=False):
    user = "postgres" if port == 55438 else "postgres.pgsprite"
    # Fixed disposable local credentials: never take a production DSN from the environment.
    url = f"postgres://{user}:pgsprite_test_only@127.0.0.1:{port}/postgres?sslmode=disable"
    if transaction:
        url += "&default_query_exec_mode=exec"
    result = subprocess.run([str(BINARY), "migrate", "--alter", statement, "--json"], env={**os.environ, "PGSPRITE_URL": url}, text=True, capture_output=True, timeout=45)
    if transaction:
        assert result.returncode != 0, "transaction pooling must not execute schema changes"
        assert sql("SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='pgsprite_service_probe' AND column_name='transaction_note';") == "0"
        return
    assert result.returncode == 0, result.stderr
    report = json.loads(result.stdout)
    assert report["outcome"] == "executed-natively", report
    return report


def token(tenant):
    def encode(value):
        return base64.urlsafe_b64encode(json.dumps(value, separators=(",", ":")).encode()).rstrip(b"=")
    payload = encode({"alg": "HS256", "typ": "JWT"}) + b"." + encode({"role": "authenticated", "sub": f"00000000-0000-0000-0000-{tenant:012d}", "exp": int(time.time()) + 300})
    signature = base64.urlsafe_b64encode(hmac.new(SECRET, payload, hashlib.sha256).digest()).rstrip(b"=")
    return (payload + b"." + signature).decode()


def api_rows(tenant, columns):
    request = urllib.request.Request("http://127.0.0.1:55439/pgsprite_service_probe?select=" + columns, headers={"Authorization": "Bearer " + token(tenant)})
    with urllib.request.urlopen(request, timeout=3) as response:
        return json.load(response)


def verify_api(columns, extra=None):
    deadline = time.monotonic() + 30
    last = None
    while time.monotonic() < deadline:
        try:
            for tenant in (1, 2):
                expected = {"id": tenant}
                if extra:
                    expected[extra] = None
                rows = api_rows(tenant, columns)
                assert rows == [expected], (tenant, rows)
            assert api_rows(3, columns) == [], "unrelated tenant saw rows"
            assert sql(f"SELECT count(*) FROM {TABLE};") == "2"
            return
        except (urllib.error.URLError, TimeoutError, AssertionError) as err:
            last = err
            time.sleep(0.2)
    raise AssertionError(f"PostgREST did not converge: {last}")


def main():
    sql(f"""
DROP TABLE IF EXISTS {TABLE};
CREATE TABLE {TABLE} (id int PRIMARY KEY, owner_id uuid NOT NULL, body text NOT NULL);
ALTER TABLE {TABLE} ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON {TABLE} TO authenticated USING (owner_id = auth.uid()) WITH CHECK (owner_id = auth.uid());
GRANT SELECT ON {TABLE} TO authenticated;
INSERT INTO {TABLE} VALUES (1,'00000000-0000-0000-0000-000000000001','first'), (2,'00000000-0000-0000-0000-000000000002','second');
NOTIFY pgrst, 'reload schema';
""")
    try:
        verify_api("id")
        change(55438, f"ALTER TABLE {TABLE} ADD COLUMN direct_note text")
        verify_api("id,direct_note", "direct_note")
        change(55440, f"ALTER TABLE {TABLE} ADD COLUMN session_note text")
        verify_api("id,session_note", "session_note")
        report = change(55440, f"CREATE INDEX pgsprite_service_probe_body ON {TABLE}(body)")
        assert len(report["executed_sql"]) == 1
        assert report["executed_sql"][0].startswith("CREATE INDEX CONCURRENTLY ")
        verify_api("id,session_note", "session_note")
        change(55441, f"ALTER TABLE {TABLE} ADD COLUMN transaction_note text", transaction=True)
        verify_api("id,session_note", "session_note")
        print("PASS: direct/session changes, concurrent index, JWT tenant isolation, automatic API cache refresh, transaction-pool refusal")
    finally:
        sql(f"DROP TABLE {TABLE};")


if __name__ == "__main__":
    main()
