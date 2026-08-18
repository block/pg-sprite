# Diff, brand-new table: absent from the live database, plans as the full
# desired schema, reported in the diagnostic grammar with the greenfield note.
# Render from this directory: vhs diff-greenfield.tape (or `make demos` at the repo root).
# Needs: make build && make db-up first. Hidden setup drops widgets so the
# tape is deterministic and rerunnable.
Output diff-greenfield.gif

Set Shell "bash"
Set FontSize 14
Set Width 1300
Set Height 800
Set Padding 16
Set TypingSpeed 30ms

Hide
Type `export PATH="$(git rev-parse --show-toplevel)/bin:$PATH"` Enter
Type "export PGSPRITE_URL='postgres://pgsprite:pgsprite@localhost:5432/pgsprite?sslmode=disable'" Enter
Type "docker exec compose-postgres-1 psql -U pgsprite -d pgsprite -c 'DROP TABLE IF EXISTS widgets' >/dev/null" Enter
Type `demo_dir="$(mktemp -d)" && cd "$demo_dir"` Enter
Type "echo 'CREATE TABLE widgets (id bigint PRIMARY KEY, name text NOT NULL, price numeric);' > widgets.sql" Enter
Type "export PS1='\n~/kiran01bm/github/pg-sprite main '" Enter
Type "clear" Enter
Sleep 1.5s
Show

Type "cat widgets.sql"
Sleep 500ms
Enter
Sleep 2s
Enter
Enter
Sleep 500ms

Type "pg-sprite diff --desired widgets.sql"
Sleep 500ms
Enter
Sleep 8s
