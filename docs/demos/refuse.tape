# Refuse with typed help and the CI exit-code contract:
# error[rewrite-required] + note[safer-idiom] + help[name-constraint-then-validate],
# doc anchors, and echo $? showing 2.
# Render from this directory: vhs refuse.tape (or `make demos` at the repo root).
# Needs: make build && make db-up first. Hidden setup reseeds the users table.
Output refuse.gif

Set Shell "bash"
Set FontSize 14
Set Width 1300
Set Height 760
Set Padding 16
Set TypingSpeed 30ms

Hide
Type `export PATH="$(git rev-parse --show-toplevel)/bin:$PATH"` Enter
Type "export PGSPRITE_URL='postgres://pgsprite:pgsprite@localhost:5432/pgsprite?sslmode=disable'" Enter
Type `docker exec compose-postgres-1 psql -U pgsprite -d pgsprite -c 'DROP TABLE IF EXISTS users' -c 'CREATE TABLE users (id bigint PRIMARY KEY, email text)' -c "INSERT INTO users SELECT g, 'u'||g||'@x.com' FROM generate_series(1,1000) g" >/dev/null` Enter
Type "export PS1='\n~/kiran01bm/github/pg-sprite main '" Enter
Type "clear" Enter
Sleep 1.5s
Show

Type "pg-sprite migrate --alter 'ALTER TABLE users ADD CHECK (id > 0)' --dry-run"
Sleep 500ms
Enter
Sleep 8s
Enter
Enter
Sleep 500ms

Type "echo $?"
Sleep 500ms
Enter
Sleep 5s
