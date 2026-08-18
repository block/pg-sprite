# Lint: offline, no database. unset PGSPRITE_URL on camera to make the point,
# then lint a two-statement change file.
# Render from this directory: vhs lint.tape (or `make demos` at the repo root).
# Needs: make build only — no database.
Output lint.gif

Set Shell "bash"
Set FontSize 14
Set Width 1560
Set Height 1160
Set Padding 16
Set TypingSpeed 30ms

Hide
Type `export PATH="$(git rev-parse --show-toplevel)/bin:$PATH"` Enter
Type "cd /tmp" Enter
Type "echo 'CREATE INDEX users_email_idx ON users (email);' > changes.sql" Enter
Type "echo 'ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);' >> changes.sql" Enter
Type "export PS1='\n~/pg-sprite main '" Enter
Type "clear" Enter
Sleep 1.5s
Show

Type "unset PGSPRITE_URL"
Sleep 500ms
Enter
Sleep 1s
Enter
Enter
Sleep 500ms

Type "cat changes.sql"
Sleep 500ms
Enter
Sleep 2s
Enter
Enter
Sleep 500ms

Type "pg-sprite lint changes.sql"
Sleep 500ms
Enter
Sleep 8s

# Cleanup off camera. mktemp -d would not leave a file behind, but lint
# prints the absolutized file path in its finding labels, so recording from
# a mktemp dir would put /var/folders noise into the GIF; /tmp keeps the
# label short and this removes the file.
Hide
Type "rm -f /tmp/changes.sql" Enter
