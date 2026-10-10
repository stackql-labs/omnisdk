package sqlfn

import (
	"strings"
	"sync"
	"unicode/utf8"
)

// The Postgres regular expression functions (regexp.c, and varlena.c's replace_text_regexp), over
// the ported engine.

// reWide is pg_mb2wchar_with_len over UTF8: a string's code points.
func reWide(s string) []reChr {
	out := make([]reChr, 0, len(s))
	for _, r := range s {
		out = append(out, reChr(r))
	}
	return out
}

// reNarrow is pg_wchar2mb_with_len over UTF8.
func reNarrow(w []reChr) string {
	var b strings.Builder
	for _, c := range w {
		b.WriteRune(rune(c))
	}
	return b.String()
}

// pgReCacheMax bounds the compiled-pattern cache, as MAX_CACHED_RES does Postgres's.
const pgReCacheMax = 32

type pgReKey struct {
	pattern string
	cflags  int
	stk     reStack
}

var (
	pgReCacheMu sync.Mutex
	pgReCache   = map[pgReKey]*reRegex{}
)

// pgReCall is where a regex function's call stands on the server's stack: the bytes in use when
// it calls pg_regcomp and when it calls pg_regexec.
type pgReCall struct {
	fr      *reFrames
	compile int
	exec    int
}

func (c pgReCall) compileStack() reStack { return reStack{fr: c.fr, used: c.compile} }

func (c pgReCall) execStack() reStack { return reStack{fr: c.fr, used: c.exec} }

// pgReCompile is RE_compile_and_cache: pattern compiled under cflags, or Postgres's error.
func pgReCompile(pattern string, cflags int, call pgReCall) (*reRegex, error) {
	key := pgReKey{pattern, cflags, call.compileStack()}
	pgReCacheMu.Lock()
	re, ok := pgReCache[key]
	pgReCacheMu.Unlock()
	if ok {
		return re, nil
	}
	re, code := reCompile(reWide(pattern), cflags, call.compileStack())
	if code != reOKAY {
		return nil, pgErrorf("invalid regular expression: %s", reErrorText(code))
	}
	pgReCacheMu.Lock()
	if len(pgReCache) >= pgReCacheMax {
		for k := range pgReCache {
			delete(pgReCache, k)
			break
		}
	}
	pgReCache[key] = re
	pgReCacheMu.Unlock()
	return re, nil
}

// pgReExec is RE_wchar_execute: whether re matches data from start, filling pmatch.
func pgReExec(re *reRegex, data []reChr, start int, pmatch []reMatch, call pgReCall) (bool, error) {
	r := reExecute(re, data, start, pmatch, 0, call.execStack())
	if r != reOKAY && r != reNOMATCH {
		return false, pgErrorf("regular expression failed: %s", reErrorText(r))
	}
	return r == reOKAY, nil
}

// pgReFlags is pg_re_flags.
type pgReFlags struct {
	cflags int
	glob   bool
}

// pgReParseFlags is parse_re_flags.
func pgReParseFlags(opts string) (pgReFlags, error) {
	f := pgReFlags{cflags: reADVANCED}
	for i := 0; i < len(opts); i++ {
		switch opts[i] {
		case 'g':
			f.glob = true
		case 'b':
			f.cflags &^= reADVANCED | reEXTENDED | reQUOTE
		case 'c':
			f.cflags &^= reICASE
		case 'e':
			f.cflags |= reEXTENDED
			f.cflags &^= reADVANCED | reQUOTE
		case 'i':
			f.cflags |= reICASE
		case 'm', 'n':
			f.cflags |= reNEWLINE
		case 'p':
			f.cflags |= reNLSTOP
			f.cflags &^= reNLANCH
		case 'q':
			f.cflags |= reQUOTE
			f.cflags &^= reADVANCED | reEXTENDED
		case 's':
			f.cflags &^= reNEWLINE
		case 't':
			f.cflags &^= reEXPANDED
		case 'w':
			f.cflags &^= reNLSTOP
			f.cflags |= reNLANCH
		case 'x':
			f.cflags |= reEXPANDED
		default:
			n := pgReMblen(opts[i])
			if i+n > len(opts) {
				n = len(opts) - i
			}
			return f, pgErrorf("invalid regular expression option: \"%s\"", opts[i:i+n])
		}
	}
	return f, nil
}

// pgReMblen is pg_mblen for UTF8: a character's length from its first byte.
func pgReMblen(c byte) int {
	switch {
	case c&0x80 == 0:
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

// pgReGlobal is the error of a function that does not take the g flag.
func pgReGlobal(fn string) error {
	return pgErrorf("%s does not support the \"global\" option", fn)
}

// pgReSubstr is textregexsubstr: the text matched by the first parenthesized subexpression, or
// by the whole pattern if it has none.
func pgReSubstr(s, p string, call pgReCall) (any, error) {
	re, err := pgReCompile(p, reADVANCED, call)
	if err != nil {
		return nil, err
	}
	data := reWide(s)
	pmatch := make([]reMatch, 2)
	ok, err := pgReExec(re, data, 0, pmatch, call)
	if err != nil || !ok {
		return nil, err
	}
	m := pmatch[0]
	if re.nsub > 0 {
		m = pmatch[1]
	}
	if m.so < 0 || m.eo < 0 {
		return nil, nil
	}
	return reNarrow(data[m.so:m.eo]), nil
}

// pgReSimilarEscape is similar_escape_internal: a SQL SIMILAR TO pattern as a POSIX one. A nil
// escape is the default backslash; an empty one is none.
func pgReSimilarEscape(pat string, esc *string) (string, error) {
	e := "\\"
	if esc != nil {
		e = *esc
		if len(e) > 1 && utf8.RuneCountInString(e) > 1 {
			return "", pgErrorf("invalid escape string")
		}
	}
	hasEsc := e != ""
	elen := len(e)
	afterescape := false
	incharclass := false
	nquotes := 0

	var r strings.Builder
	r.WriteString("^(?:")
	for i := 0; i < len(pat); {
		pchar := pat[i]
		if elen > 1 {
			mblen := pgReMblen(pchar)
			if mblen > 1 {
				if i+mblen > len(pat) {
					mblen = len(pat) - i
				}
				ch := pat[i : i+mblen]
				if afterescape {
					r.WriteByte('\\')
					r.WriteString(ch)
					afterescape = false
				} else if hasEsc && elen == mblen && ch == e {
					afterescape = true
				} else {
					r.WriteString(ch)
				}
				i += mblen
				continue
			}
		}

		if afterescape {
			if pchar == '"' && !incharclass {
				switch nquotes {
				case 0:
					r.WriteString("){1,1}?(")
				case 1:
					r.WriteString("){1,1}(?:")
				default:
					return "", pgErrorf("SQL regular expression may not contain more than two escape-double-quote separators")
				}
				nquotes++
			} else {
				r.WriteByte('\\')
				r.WriteByte(pchar)
			}
			afterescape = false
		} else if hasEsc && pchar == e[0] {
			afterescape = true
		} else if incharclass {
			if pchar == '\\' {
				r.WriteByte('\\')
			}
			r.WriteByte(pchar)
			if pchar == ']' {
				incharclass = false
			}
		} else if pchar == '[' {
			r.WriteByte(pchar)
			incharclass = true
		} else if pchar == '%' {
			r.WriteString(".*")
		} else if pchar == '_' {
			r.WriteByte('.')
		} else if pchar == '(' {
			r.WriteString("(?:")
		} else if pchar == '\\' || pchar == '.' || pchar == '^' || pchar == '$' {
			r.WriteByte('\\')
			r.WriteByte(pchar)
		} else {
			r.WriteByte(pchar)
		}
		i++
	}
	r.WriteString(")$")
	return r.String(), nil
}

// pgReMatches is regexp_matches_ctx: every match's locations, in characters.
type pgReMatches struct {
	wide      []reChr
	nmatches  int
	npatterns int
	locs      []int
}

// pgReSetupMatches is setup_regexp_matches.
func pgReSetupMatches(s, pattern string, f pgReFlags, useSubpatterns, ignoreDegenerate bool,
	call pgReCall) (*pgReMatches, error) {
	m := &pgReMatches{wide: reWide(s)}
	wideLen := len(m.wide)
	re, err := pgReCompile(pattern, f.cflags, call)
	if err != nil {
		return nil, err
	}
	var pmatchLen int
	if useSubpatterns && re.nsub > 0 {
		m.npatterns = re.nsub
		pmatchLen = re.nsub + 1
	} else {
		useSubpatterns = false
		m.npatterns = 1
		pmatchLen = 1
	}
	pmatch := make([]reMatch, pmatchLen)

	arrayLen := 31
	if f.glob {
		arrayLen = 255
	}
	prevMatchEnd := 0
	startSearch := 0
	for {
		ok, err := pgReExec(re, m.wide, startSearch, pmatch, call)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if !ignoreDegenerate || (pmatch[0].so < wideLen && pmatch[0].eo > prevMatchEnd) {
			for len(m.locs)+m.npatterns*2+1 > arrayLen {
				arrayLen += arrayLen + 1
				if arrayLen > pgMaxAlloc/4 {
					return nil, pgErrorf("too many regular expression matches")
				}
			}
			if useSubpatterns {
				for i := 1; i <= m.npatterns; i++ {
					m.locs = append(m.locs, pmatch[i].so, pmatch[i].eo)
				}
			} else {
				m.locs = append(m.locs, pmatch[0].so, pmatch[0].eo)
			}
			m.nmatches++
		}
		prevMatchEnd = pmatch[0].eo

		if !f.glob {
			break
		}
		startSearch = prevMatchEnd
		if pmatch[0].so == pmatch[0].eo {
			startSearch++
		}
		if startSearch > wideLen {
			break
		}
	}
	m.locs = append(m.locs, wideLen)
	return m, nil
}

// matchResult is build_regexp_match_result: match n's subpattern texts.
func (m *pgReMatches) matchResult(n int) pgTextArr {
	out := make(pgTextArr, m.npatterns)
	loc := n * m.npatterns * 2
	for i := 0; i < m.npatterns; i++ {
		so, eo := m.locs[loc], m.locs[loc+1]
		loc += 2
		if so < 0 || eo < 0 {
			continue
		}
		t := reNarrow(m.wide[so:eo])
		out[i] = &t
	}
	return out
}

// splitResult is build_regexp_split_result: the text between match n-1 and match n.
func (m *pgReMatches) splitResult(n int) (string, error) {
	startpos := 0
	if n > 0 {
		startpos = m.locs[n*2-1]
	}
	if startpos < 0 {
		return "", pgErrorf("invalid match ending position")
	}
	endpos := m.locs[n*2]
	if endpos < startpos {
		return "", pgErrorf("invalid match starting position")
	}
	return reNarrow(m.wide[startpos:endpos]), nil
}

func pgReOptFlags(a []any) (pgReFlags, error) {
	if len(a) > 2 {
		return pgReParseFlags(a[2].(string))
	}
	return pgReParseFlags("")
}

// pgReMatch is regexp_match.
func pgReMatch(a []any, call pgReCall) (any, error) {
	f, err := pgReOptFlags(a)
	if err != nil {
		return nil, err
	}
	if f.glob {
		return nil, pgReGlobal("regexp_match()")
	}
	m, err := pgReSetupMatches(a[0].(string), a[1].(string), f, true, false, call)
	if err != nil {
		return nil, err
	}
	if m.nmatches == 0 {
		return nil, nil
	}
	return m.matchResult(0), nil
}

// pgReMatchesSRF is regexp_matches.
func pgReMatchesSRF(a []any, call pgReCall) (any, error) {
	f, err := pgReOptFlags(a)
	if err != nil {
		return nil, err
	}
	m, err := pgReSetupMatches(a[0].(string), a[1].(string), f, true, false, call)
	if err != nil {
		return nil, err
	}
	rows := pgRows{}
	for i := 0; i < m.nmatches; i++ {
		rows = append(rows, []any{m.matchResult(i)})
	}
	return rows, nil
}

// pgReSplit is regexp_split_to_table's and regexp_split_to_array's matching.
func pgReSplit(a []any, fn string, call pgReCall) ([]string, error) {
	f, err := pgReOptFlags(a)
	if err != nil {
		return nil, err
	}
	if f.glob {
		return nil, pgReGlobal(fn)
	}
	f.glob = true
	m, err := pgReSetupMatches(a[0].(string), a[1].(string), f, false, true, call)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, m.nmatches+1)
	for i := 0; i <= m.nmatches; i++ {
		s, err := m.splitResult(i)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// pgReReplace is replace_text_regexp.
func pgReReplace(src string, re *reRegex, repl string, glob bool, call pgReCall) (string, error) {
	data := reWide(src)
	haveEscape := strings.IndexByte(repl, '\\') >= 0
	var buf strings.Builder
	pmatch := make([]reMatch, 10)
	dataPos := 0
	searchStart := 0
	for searchStart <= len(data) {
		r := reExecute(re, data, searchStart, pmatch, 0, call.execStack())
		if r == reNOMATCH {
			break
		}
		if r != reOKAY {
			return "", pgErrorf("regular expression failed: %s", reErrorText(r))
		}
		if pmatch[0].so-dataPos > 0 {
			buf.WriteString(reNarrow(data[dataPos:pmatch[0].so]))
			dataPos = pmatch[0].so
		}
		if haveEscape {
			pgReAppendSubstr(&buf, repl, pmatch, data)
		} else {
			buf.WriteString(repl)
		}
		dataPos = pmatch[0].eo
		if !glob {
			break
		}
		searchStart = dataPos
		if pmatch[0].so == pmatch[0].eo {
			searchStart++
		}
	}
	if dataPos < len(data) {
		buf.WriteString(reNarrow(data[dataPos:]))
	}
	return buf.String(), nil
}

// pgReAppendSubstr is appendStringInfoRegexpSubstr: the replacement with \1..\9, \& and \\
// substituted.
func pgReAppendSubstr(buf *strings.Builder, repl string, pmatch []reMatch, data []reChr) {
	p := 0
	for {
		chunkStart := p
		for p < len(repl) && repl[p] != '\\' {
			p++
		}
		if p > chunkStart {
			buf.WriteString(repl[chunkStart:p])
		}
		if p >= len(repl) {
			break
		}
		p++
		if p >= len(repl) {
			buf.WriteByte('\\')
			break
		}
		var so, eo int
		switch c := repl[p]; {
		case c >= '1' && c <= '9':
			idx := int(c - '0')
			so, eo = pmatch[idx].so, pmatch[idx].eo
			p++
		case c == '&':
			so, eo = pmatch[0].so, pmatch[0].eo
			p++
		case c == '\\':
			buf.WriteByte('\\')
			p++
			continue
		default:
			buf.WriteByte('\\')
			continue
		}
		if so != -1 && eo != -1 {
			buf.WriteString(reNarrow(data[so:eo]))
		}
	}
}

// pgReEntry is a regex function's own frames on the way to pg_regcomp and pg_regexec, by build:
// the fmgr function's, then RE_compile_and_cache's, setup_regexp_matches's, RE_wchar_execute's or
// replace_text_regexp's, with the arguments pushed for their calls.
type pgReEntry struct{ compile, exec int }

var pgReEntries = map[bool]map[string]pgReEntry{
	false: {
		"substring": {128 + 304, 128 + 176}, "regexp_match": {64 + 16 + 144 + 304, 64 + 16 + 144 + 176},
		"regexp_matches":        {96 + 16 + 144 + 304, 96 + 16 + 144 + 176},
		"regexp_split_to_table": {96 + 16 + 144 + 304, 96 + 16 + 144 + 176},
		"regexp_split_to_array": {64 + 16 + 144 + 304, 64 + 16 + 144 + 176},
		"regexp_replace3":       {48 + 304, 464 + 16}, "regexp_replace4": {64 + 304, 64 + 464 + 16},
	},
	true: {
		"substring": {112 + 304, 112 + 160}, "regexp_match": {64 + 160 + 304, 64 + 160 + 160},
		"regexp_matches":        {80 + 160 + 304, 80 + 160 + 160},
		"regexp_split_to_table": {96 + 160 + 304, 96 + 160 + 160},
		"regexp_split_to_array": {64 + 160 + 304, 64 + 160 + 160},
		"regexp_replace3":       {48 + 304, 464}, "regexp_replace4": {64 + 304, 64 + 464},
	},
}

// pgReBase is the stack in use below a regex function's frame, as measured against the server:
// for a scalar call, which Postgres folds while planning when its arguments are constants, and for
// a set-returning one scanned by the executor. Elsewhere in a query the depth differs by the
// frames between, so the boundary of what is too complex moves by as many levels as those take.
var pgReBase = map[bool]struct{ scalar, srf int }{
	false: {scalar: 0, srf: 0},
	true:  {scalar: 0, srf: 0},
}

// pgReCallFor is where fn's call stands on env's server.
func pgReCallFor(env pgEnv, fn string, srf bool) pgReCall {
	arm := env.contract // the aarch64 build is the one that contracts
	fr := &reFramesAMD64
	if arm {
		fr = &reFramesARM64
	}
	base := pgReBase[arm].scalar
	if srf {
		base = pgReBase[arm].srf
	}
	e := pgReEntries[arm][fn]
	return pgReCall{fr: fr, compile: base + e.compile, exec: base + e.exec}
}

func pgReRegister(sig, fn string, srf bool, f func(a []any, call pgReCall) (any, error)) {
	registerPgEnv(sig, func(env pgEnv, a []any) (any, error) { return f(a, pgReCallFor(env, fn, srf)) })
}

func init() {
	pgReRegister("substring(text,text)", "substring", false, func(a []any, call pgReCall) (any, error) {
		return pgReSubstr(a[0].(string), a[1].(string), call)
	})
	pgReRegister("substring(text,text,text)", "substring", false, func(a []any, call pgReCall) (any, error) {
		esc := a[2].(string)
		p, err := pgReSimilarEscape(a[1].(string), &esc)
		if err != nil {
			return nil, err
		}
		return pgReSubstr(a[0].(string), p, call)
	})
	pgReRegister("regexp_match(text,text)", "regexp_match", false, pgReMatch)
	pgReRegister("regexp_match(text,text,text)", "regexp_match", false, pgReMatch)
	pgReRegister("regexp_matches(text,text)", "regexp_matches", true, pgReMatchesSRF)
	pgReRegister("regexp_matches(text,text,text)", "regexp_matches", true, pgReMatchesSRF)
	replace := func(a []any, call pgReCall) (any, error) {
		f, err := pgReParseFlags("")
		if len(a) > 3 {
			f, err = pgReParseFlags(a[3].(string))
		}
		if err != nil {
			return nil, err
		}
		re, err := pgReCompile(a[1].(string), f.cflags, call)
		if err != nil {
			return nil, err
		}
		return pgReReplace(a[0].(string), re, a[2].(string), f.glob, call)
	}
	pgReRegister("regexp_replace(text,text,text)", "regexp_replace3", false, replace)
	pgReRegister("regexp_replace(text,text,text,text)", "regexp_replace4", false, replace)
	splitTable := func(a []any, call pgReCall) (any, error) {
		parts, err := pgReSplit(a, "regexp_split_to_table()", call)
		if err != nil {
			return nil, err
		}
		rows := make(pgRows, len(parts))
		for i, s := range parts {
			rows[i] = []any{s}
		}
		return rows, nil
	}
	pgReRegister("regexp_split_to_table(text,text)", "regexp_split_to_table", true, splitTable)
	pgReRegister("regexp_split_to_table(text,text,text)", "regexp_split_to_table", true, splitTable)
	splitArray := func(a []any, call pgReCall) (any, error) {
		parts, err := pgReSplit(a, "regexp_split_to_array()", call)
		if err != nil {
			return nil, err
		}
		out := make(pgTextArr, len(parts))
		for i := range parts {
			out[i] = &parts[i]
		}
		return out, nil
	}
	pgReRegister("regexp_split_to_array(text,text)", "regexp_split_to_array", false, splitArray)
	pgReRegister("regexp_split_to_array(text,text,text)", "regexp_split_to_array", false, splitArray)
}
