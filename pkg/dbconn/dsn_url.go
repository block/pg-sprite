package dbconn

import "strings"

// urlDSNSSLMode returns the sslmode a postgres:// or postgresql:// URL leaves
// in effect, or "" when it selects none or does not parse.
//
// The scan follows parseURLSettings and parseURLQueryParams in pgx v5.12.0's
// pgconn/parse_url.go, which match libpq's URI parser (copied rather than
// imported: pgx does not export them). Every component is decoded, not just
// the query, because one that fails to decode fails the whole URL. The rules
// that decide which sslmode survives:
//
//   - A `@` anywhere before the first `/` ends the user info, even one inside
//     what looks like a query.
//   - Query pairs are split on `&`; each needs exactly one `=`. Both halves
//     are percent-decoded: `%` takes exactly two hex digits, `%00` is
//     refused, leading and trailing spaces are dropped and inner ones refused.
//     `+` and `#` are ordinary characters.
//   - When a key repeats, its last value wins.
//   - `ssl=true` as the final ssl value selects sslmode=require, unless an
//     explicit sslmode comes after it.
//
// pgx's only key alias, dbname, names neither key, so keys are compared as
// decoded.
func urlDSNSSLMode(dsn string) string {
	if strings.IndexByte(dsn, 0) >= 0 {
		return ""
	}
	p, ok := strings.CutPrefix(dsn, "postgresql://")
	if !ok {
		p, ok = strings.CutPrefix(dsn, "postgres://")
	}
	if !ok {
		return ""
	}
	p, ok = skipURLUserInfo(p)
	if !ok {
		return ""
	}
	p, ok = skipURLHostList(p)
	if !ok {
		return ""
	}
	if path, isPath := strings.CutPrefix(p, "/"); isPath {
		dbname, query, hasQuery := strings.Cut(path, "?")
		if _, ok := uriDecode(dbname); !ok {
			return ""
		}
		p = ""
		if hasQuery {
			p = "?" + query
		}
	}
	query, hasQuery := strings.CutPrefix(p, "?")
	if !hasQuery {
		return ""
	}
	return urlQuerySSLMode(query)
}

// skipURLUserInfo consumes the `user:password@` designator, if the URL has
// one, and reports whether both halves decode.
func skipURLUserInfo(p string) (rest string, ok bool) {
	i := strings.IndexAny(p, "@/")
	if i < 0 || p[i] != '@' {
		return p, true
	}
	user, password, _ := strings.Cut(p[:i], ":")
	if _, ok := uriDecode(user); !ok {
		return "", false
	}
	if _, ok := uriDecode(password); !ok {
		return "", false
	}
	return p[i+1:], true
}

// skipURLHostList consumes the comma-separated host[:port] list and reports
// whether it parses. Hosts and ports are each decoded as one comma-joined
// list, as pgx does, so a space beside an inner comma fails the URL.
func skipURLHostList(p string) (rest string, ok bool) {
	var hosts, ports strings.Builder
	for {
		if strings.HasPrefix(p, "[") {
			end := strings.IndexByte(p, ']')
			if end <= 1 {
				// Unterminated, or an empty IPv6 address.
				return "", false
			}
			hosts.WriteString(p[1:end])
			p = p[end+1:]
			if p != "" && strings.IndexByte(":/?,", p[0]) < 0 {
				return "", false
			}
		} else {
			i := strings.IndexAny(p, ":/?,")
			if i < 0 {
				i = len(p)
			}
			hosts.WriteString(p[:i])
			p = p[i:]
		}
		if port, isPort := strings.CutPrefix(p, ":"); isPort {
			i := strings.IndexAny(port, "/?,")
			if i < 0 {
				i = len(port)
			}
			ports.WriteString(port[:i])
			p = port[i:]
		}
		next, more := strings.CutPrefix(p, ",")
		if !more {
			break
		}
		p = next
		hosts.WriteByte(',')
		ports.WriteByte(',')
	}
	if _, ok := uriDecode(hosts.String()); !ok {
		return "", false
	}
	if _, ok := uriDecode(ports.String()); !ok {
		return "", false
	}
	return p, true
}

// urlQuerySSLMode returns the sslmode the query string (after its `?`)
// leaves in effect, or "" when it selects none or does not parse.
func urlQuerySSLMode(query string) string {
	settings := map[string]string{}
	sslWasLast := false
	for query != "" {
		var pair string
		pair, query, _ = strings.Cut(query, "&")
		rawKey, rawValue, found := strings.Cut(pair, "=")
		if !found || strings.Contains(rawValue, "=") {
			return ""
		}
		key, ok := uriDecode(rawKey)
		if !ok {
			return ""
		}
		value, ok := uriDecode(rawValue)
		if !ok {
			return ""
		}
		switch key {
		case "ssl":
			sslWasLast = true
		case "sslmode":
			sslWasLast = false
		}
		settings[key] = value
	}
	if settings["ssl"] == "true" && sslWasLast {
		return "require"
	}
	return settings["sslmode"]
}

// uriDecode percent-decodes one URI component with libpq's rules, listed on
// urlDSNSSLMode, and reports whether it decodes.
func uriDecode(raw string) (string, bool) {
	var b strings.Builder
	b.Grow(len(raw))
	i := 0
	for i < len(raw) && raw[i] == ' ' {
		i++
	}
	for i < len(raw) && raw[i] != ' ' {
		if raw[i] != '%' {
			b.WriteByte(raw[i])
			i++
			continue
		}
		if i+2 >= len(raw) {
			return "", false
		}
		hi, hiOK := hexDigit(raw[i+1])
		lo, loOK := hexDigit(raw[i+2])
		if !hiOK || !loOK {
			return "", false
		}
		c := hi<<4 | lo
		if c == 0 {
			return "", false
		}
		b.WriteByte(c)
		i += 3
	}
	for i < len(raw) && raw[i] == ' ' {
		i++
	}
	if i < len(raw) {
		return "", false
	}
	return b.String(), true
}

// hexDigit returns the value of one hexadecimal digit of either case.
func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}
