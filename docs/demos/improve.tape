# Improve (the README hero): dry-run, real run, proof in the catalog.
# Render from this directory: vhs improve.tape (or `make demos` at the repo root).
# Needs: make build && make db-up first. Hidden setup reseeds the users table,
# so the tape is deterministic and rerunnable.
Output improve.gif

Set Shell "bash"
Set FontSize 14
Set Width 1300
Set Height 980
Set Padding 16
Set TypingSpeed 30ms

Hide
Type `export PATH="$(git rev-parse --show-toplevel)/bin:$PATH"` Enter
Type "export PGSPRITE_URL='postgres://pgsprite:pgsprite@localhost:5432/pgsprite?sslmode=disable'" Enter
Type `docker exec compose-postgres-1 psql -U pgsprite -d pgsprite -c 'DROP TABLE IF EXISTS users' -c 'CREATE TABLE users (id bigint PRIMARY KEY, email text)' -c "INSERT INTO users SELECT g, 'u'||g||'@x.com' FROM generate_series(1,1000) g" >/dev/null` Enter
Type "export PS1='~/kiran01bm/github/pg-sprite main '" Enter
Type "clear" Enter
Sleep 1.5s
Show

Type "pg-sprite migrate --alter 'ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)' --dry-run"
Sleep 500ms
Enter
Sleep 6s

Type "pg-sprite migrate --alter 'ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)'"
Sleep 500ms
Enter
Sleep 10s

Type "docker exec compose-postgres-1 psql -U pgsprite -d pgsprite -c '\d users'"
Sleep 500ms
Enter
Sleep 8s
