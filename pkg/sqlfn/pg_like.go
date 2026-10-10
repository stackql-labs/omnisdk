package sqlfn

// Postgres's LIKE (like.c, like_match.c) in UTF8 with the C collation: textlike, behind the ~~
// operator and the function like(string, pattern), and like_escape, which rewrites a pattern for
// an ESCAPE clause into one that escapes with backslash.

const (
	pgLikeFalse = 0
	pgLikeTrue  = 1
	pgLikeAbort = -1
)

var errLikeEscape = pgErrorf("LIKE pattern must not end with escape character")

// pgCharLen is NextChar's step in UTF8: the length the first byte announces.
func pgCharLen(s string, i int) int {
	c := s[i]
	switch {
	case c < 0x80:
		return 1
	case c&0xe0 == 0xc0:
		return 2
	case c&0xf0 == 0xe0:
		return 3
	case c&0xf8 == 0xf0:
		return 4
	}
	return 1
}

// pgMatchText is UTF8_MatchText: t against pattern p, % and _ wild, backslash escaping.
func pgMatchText(t string, ti int, p string, pi int) (int, error) {
	if len(p)-pi == 1 && p[pi] == '%' {
		return pgLikeTrue, nil
	}
	for ti < len(t) && pi < len(p) {
		switch p[pi] {
		case '\\':
			pi++
			if pi >= len(p) {
				return 0, errLikeEscape
			}
			if p[pi] != t[ti] {
				return pgLikeFalse, nil
			}
		case '%':
			pi++
			for pi < len(p) {
				if p[pi] == '%' {
					pi++
				} else if p[pi] == '_' {
					if ti >= len(t) {
						return pgLikeAbort, nil
					}
					ti += pgCharLen(t, ti)
					pi++
				} else {
					break
				}
			}
			if pi >= len(p) {
				return pgLikeTrue, nil
			}
			var firstpat byte
			if p[pi] == '\\' {
				if len(p)-pi < 2 {
					return 0, errLikeEscape
				}
				firstpat = p[pi+1]
			} else {
				firstpat = p[pi]
			}
			for ti < len(t) {
				if t[ti] == firstpat {
					matched, err := pgMatchText(t, ti, p, pi)
					if err != nil {
						return 0, err
					}
					if matched != pgLikeFalse {
						return matched, nil
					}
				}
				ti += pgCharLen(t, ti)
			}
			return pgLikeAbort, nil
		case '_':
			ti += pgCharLen(t, ti)
			pi++
			continue
		default:
			if p[pi] != t[ti] {
				return pgLikeFalse, nil
			}
		}
		ti++
		pi++
	}
	if ti < len(t) {
		return pgLikeFalse, nil
	}
	for pi < len(p) && p[pi] == '%' {
		pi++
	}
	if pi >= len(p) {
		return pgLikeTrue, nil
	}
	return pgLikeAbort, nil
}

// pgLikeEscape is MB_do_like_escape: pat with esc as its escape character rewritten to escape with
// backslash; an empty esc means no escaping, so the pattern's backslashes become literal.
func pgLikeEscape(pat, esc string) (string, error) {
	var r []byte
	if esc == "" {
		for i := 0; i < len(pat); {
			if pat[i] == '\\' {
				r = append(r, '\\')
			}
			n := pgCharLen(pat, i)
			r = append(r, pat[i:i+n]...)
			i += n
		}
		return string(r), nil
	}
	if pgCharLen(esc, 0) != len(esc) {
		return "", pgErrorf("invalid escape string")
	}
	if esc == "\\" {
		return pat, nil
	}
	after := false
	for i := 0; i < len(pat); {
		n := pgCharLen(pat, i)
		switch {
		case pat[i:min(i+n, len(pat))] == esc && !after:
			r = append(r, '\\')
			after = true
		case pat[i] == '\\':
			r = append(r, '\\')
			if !after {
				r = append(r, '\\')
			}
			after = false
		default:
			r = append(r, pat[i:i+n]...)
			after = false
		}
		i += n
	}
	return string(r), nil
}

func init() {
	registerPg("like(text,text)", func(a []any) (any, error) {
		m, err := pgMatchText(a[0].(string), 0, a[1].(string), 0)
		if err != nil {
			return nil, err
		}
		return m == pgLikeTrue, nil
	})
	registerPg("like_escape(text,text)", func(a []any) (any, error) { return pgLikeEscape(a[0].(string), a[1].(string)) })
}
