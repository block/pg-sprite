# Improve (the README hero): dry-run, real run, proof in the catalog — then
# the same change declaratively: diff plans from a desired-state file,
# migrate executes, diff confirms convergence.
# Render from this directory: vhs improve.tape (or `make demos` at the repo root).
# Needs: make build && make db-up first. Hidden setup reseeds the users table
# and writes the desired-state file, so the tape is deterministic and
# rerunnable.
Output improve.gif

Set Shell "bash"
Set FontSize 14
Set Width 1300
Set Height 1160
Set Padding 16
Set TypingSpeed 30ms
Set Theme "Dracula"

Hide
Type `export PATH="$(git rev-parse --show-toplevel)/bin:$PATH"` Enter
# The rendering machine's environment leaks into the recorded shell: strip
# color suppressors so the report's --color=auto detection sees a normal
# terminal and the GIF shows the diagnostic colors deterministically.
Type "unset NO_COLOR && export TERM=xterm-256color" Enter
Type "export PGSPRITE_URL='postgres://pgsprite:pgsprite@localhost:5432/pgsprite?sslmode=disable'" Enter
Type `docker exec compose-postgres-1 psql -U pgsprite -d pgsprite -c 'DROP TABLE IF EXISTS users' -c 'CREATE TABLE users (id bigint PRIMARY KEY, email text)' -c "INSERT INTO users SELECT g, 'u'||g||'@x.com' FROM generate_series(1,1000) g" >/dev/null` Enter
Type `demo_dir="$(mktemp -d)" && cd "$demo_dir"` Enter
Type "echo 'CREATE TABLE users (' > users.sql" Enter
Type "echo '    id bigint PRIMARY KEY,' >> users.sql" Enter
Type "echo '    email text,' >> users.sql" Enter
Type "echo '    nickname text,' >> users.sql" Enter
Type "echo '    CONSTRAINT users_email_key UNIQUE (email)' >> users.sql" Enter
Type "echo ');' >> users.sql" Enter
# Prompt: directory cyan, branch magenta; typed input bright white. The DEBUG
# trap resets the color before each command runs, so output renders in the
# tool's own colors (the diagnostic labels) rather than the input style.
Type "export PS1='\n\[\e[1;36m\]~/kiran01bm/github/pg-sprite\[\e[0m\] \[\e[1;35m\]main\[\e[0m\] \[\e[0;97m\]'" Enter
Type `trap 'printf "\033[0m"' DEBUG` Enter
Type "clear" Enter
Sleep 1.5s
Show

Type "pg-sprite migrate --alter 'ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)' --dry-run"
Sleep 500ms
Enter
Sleep 6s
Enter
Enter
Sleep 500ms

Type "pg-sprite migrate --alter 'ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email)'"
Sleep 500ms
Enter
Sleep 10s
Enter
Enter
Sleep 500ms

Type "docker exec compose-postgres-1 psql -U pgsprite -d pgsprite -c '\d users'"
Sleep 500ms
Enter
Sleep 8s
Enter
Enter
Sleep 500ms

Type "clear"
Sleep 500ms
Enter
Sleep 1s

Type "cat users.sql"
Sleep 500ms
Enter
Sleep 2s
Enter
Enter
Sleep 500ms

Type "pg-sprite diff --desired users.sql"
Sleep 500ms
Enter
Sleep 6s
Enter
Enter
Sleep 500ms

Type "pg-sprite migrate --alter 'ALTER TABLE users ADD COLUMN nickname text'"
Sleep 500ms
Enter
Sleep 6s
Enter
Enter
Sleep 500ms

Type "pg-sprite diff --desired users.sql"
Sleep 500ms
Enter
Sleep 8s
