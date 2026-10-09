package sqlfn

import (
	"errors"
	"fmt"
)

// SQLite's JSON SQL functions over the JSONB core in sqlite_jsonb.go: json.c's function bodies, one for
// one. Each jsonb_* twin is its json_* function returning JSONB.

// Flags json.c passes its functions as user data.
const (
	jsonFlagJSON   = 0x01 // result is always JSON (->)
	jsonFlagSQL    = 0x02 // result is always SQL (->>)
	jsonFlagABPath = 0x03 // abbreviated paths allowed (-> and ->>)
	jsonFlagIsSet  = 0x04 // json_set, not json_insert
	jsonFlagAIns   = 0x08 // json_array_insert, not json_insert
	jsonFlagBlob   = 0x10 // the result is JSONB
)

func sqliteJSON() []Func {
	fn := func(name string, lo, hi int, flags int, f func(a []any, flags int) (any, error)) Func {
		return NewScalar(name, lo, hi, func(a []any) (any, error) { return f(a, flags) })
	}
	return []Func{
		fn("json", 1, 1, 0, jsonRemoveFunc),
		fn("jsonb", 1, 1, jsonFlagBlob, jsonRemoveFunc),
		fn("json_array", 0, -1, 0, jsonArrayFunc),
		fn("jsonb_array", 0, -1, jsonFlagBlob, jsonArrayFunc),
		fn("json_array_insert", 0, -1, jsonFlagAIns, jsonSetFunc),
		fn("jsonb_array_insert", 0, -1, jsonFlagAIns|jsonFlagBlob, jsonSetFunc),
		fn("json_array_length", 1, 2, 0, jsonArrayLengthFunc),
		fn("json_error_position", 1, 1, 0, jsonErrorFunc),
		fn("json_extract", 0, -1, 0, jsonExtractFunc),
		fn("jsonb_extract", 0, -1, jsonFlagBlob, jsonExtractFunc),
		fn("->", 2, 2, jsonFlagJSON, jsonExtractFunc),
		fn("->>", 2, 2, jsonFlagSQL, jsonExtractFunc),
		fn("json_insert", 0, -1, 0, jsonSetFunc),
		fn("jsonb_insert", 0, -1, jsonFlagBlob, jsonSetFunc),
		fn("json_object", 0, -1, 0, jsonObjectFunc),
		fn("jsonb_object", 0, -1, jsonFlagBlob, jsonObjectFunc),
		fn("json_patch", 2, 2, 0, jsonPatchFunc),
		fn("jsonb_patch", 2, 2, jsonFlagBlob, jsonPatchFunc),
		fn("json_pretty", 1, 2, 0, jsonPrettyFunc),
		fn("json_quote", 1, 1, 0, jsonQuoteFunc),
		fn("json_remove", 0, -1, 0, jsonRemoveFunc),
		fn("jsonb_remove", 0, -1, jsonFlagBlob, jsonRemoveFunc),
		fn("json_replace", 0, -1, 0, jsonReplaceFunc),
		fn("jsonb_replace", 0, -1, jsonFlagBlob, jsonReplaceFunc),
		fn("json_set", 0, -1, jsonFlagIsSet, jsonSetFunc),
		fn("jsonb_set", 0, -1, jsonFlagIsSet|jsonFlagBlob, jsonSetFunc),
		fn("json_type", 1, 2, 0, jsonTypeFunc),
		fn("json_valid", 1, 2, 0, jsonValidFunc),
		jsonEachTable("json_each", 1, false),
		jsonEachTable("json_tree", 1, true),
		jsonEachTable("jsonb_each", 2, false),
		jsonEachTable("jsonb_tree", 2, true),
	}
}

func jsonQuoteFunc(a []any, _ int) (any, error) {
	var jx jsonString
	jx.appendSqlValue(a[0])
	return jx.result(false)
}

func jsonArrayFunc(a []any, flags int) (any, error) {
	var jx jsonString
	jx.appendChar('[')
	for _, v := range a {
		jx.appendSeparator()
		jx.appendSqlValue(v)
	}
	jx.appendChar(']')
	return jx.result(flags&jsonFlagBlob != 0)
}

func jsonArrayLengthFunc(a []any, _ int) (any, error) {
	p, err := jsonParseFuncArg(a[0], 0)
	if p == nil {
		return nil, err
	}
	i := 0
	if len(a) == 2 {
		if isNull(a[1]) {
			return nil, nil
		}
		path := sqlText(a[1])
		if at(path, 0) == '$' {
			i = p.lookupStep(0, path, 1, 0)
		} else {
			i = p.lookupStep(0, "@", 0, 0)
		}
		if lookupIsError(i) {
			if i == lookupNotFound {
				return nil, nil
			}
			return nil, jsonBadPathError(beforeNUL(path), i)
		}
	}
	if p.aBlob[i]&0x0f == jsonbArray {
		return int64(p.arrayCount(i)), nil
	}
	return int64(0), nil
}

func jsonAllAlphanum(z string) bool {
	for i := 0; i < len(z); i++ {
		if !sqlIsAlnum(z[i]) && z[i] != '_' {
			return false
		}
	}
	return true
}

func jsonExtractFunc(a []any, flags int) (any, error) {
	if len(a) < 2 {
		return nil, nil
	}
	p, err := jsonParseFuncArg(a[0], 0)
	if p == nil {
		return nil, err
	}
	var jx jsonString
	if len(a) > 2 {
		jx.appendChar('[')
	}
	for i := 1; i < len(a); i++ {
		if isNull(a[i]) {
			return nil, nil
		}
		path := beforeNUL(sqlText(a[i]))
		var j int
		switch {
		case at(path, 0) == '$':
			j = p.lookupStep(0, path, 1, 0)
		case flags&jsonFlagABPath != 0:
			// The -> and ->> operators take abbreviated paths, as PostgreSQL does: NUMBER is $[NUMBER]
			// (counted from the end when negative), LABEL is $.LABEL, [NUMBER] is $[NUMBER].
			var ab string
			c, _ := norm(a[i])
			switch {
			case c == classInteger:
				ab = "["
				if at(path, 0) == '-' {
					ab += "#"
				}
				ab += path + "]"
			case jsonAllAlphanum(path):
				ab = "." + path
			case at(path, 0) == '[' && len(path) >= 3 && path[len(path)-1] == ']':
				ab = path
			default:
				ab = ".\"" + path + "\""
			}
			j = p.lookupStep(0, ab, 0, 0)
		default:
			return nil, jsonBadPathError(path, 0)
		}
		switch {
		case j < p.nBlob():
			if len(a) == 2 {
				if flags&jsonFlagJSON != 0 {
					var s jsonString
					p.translateBlobToText(j, &s)
					return s.result(false)
				}
				eMode := 1
				if flags&jsonFlagBlob != 0 {
					eMode = 2
				}
				v, err := p.returnFromBlob(j, eMode)
				if err != nil {
					return nil, err
				}
				if s, ok := v.(jsonText); ok && flags&jsonFlagSQL != 0 {
					return string(s), nil
				}
				return v, nil
			}
			jx.appendSeparator()
			p.translateBlobToText(j, &jx)
		case j == lookupNotFound:
			if len(a) == 2 {
				return nil, nil
			}
			jx.appendSeparator()
			jx.appendRaw("null")
		default:
			return nil, jsonBadPathError(path, j)
		}
	}
	jx.appendChar(']')
	return jx.result(flags&jsonFlagBlob != 0)
}

// Results of jsonMergePatch.
const (
	jsonMergeOK = iota
	jsonMergeBadTarget
	jsonMergeBadPatch
	jsonMergeOOM
	jsonMergeTooDeep
)

// jsonMergePatch is RFC-7396 MergePatch of the patch at iPatch into the target at iTarget, in place.
func jsonMergePatch(t *jsonParse, iTarget int, pp *jsonParse, iPatch int, iDepth int) int {
	x := pp.aBlob[iPatch] & 0x0f
	if x != jsonbObject {
		n, sz := pp.payloadSize(iPatch)
		szPatch := n + sz
		n, sz = t.payloadSize(iTarget)
		szTarget := n + sz
		t.blobEdit(iTarget, szTarget, pp.aBlob[iPatch:iPatch+szPatch], 0)
		return jsonMergeOK
	}
	x = t.aBlob[iTarget] & 0x0f
	if x != jsonbObject {
		n, sz := t.payloadSize(iTarget)
		t.blobEdit(iTarget+n, sz, nil, 0)
		x := t.aBlob[iTarget]
		t.aBlob[iTarget] = x&0xf0 | jsonbObject
	}
	n, sz := pp.payloadSize(iPatch)
	if n == 0 {
		return jsonMergeBadPatch
	}
	iPCursor := iPatch + n
	iPEnd := iPCursor + sz
	n, sz = t.payloadSize(iTarget)
	if n == 0 {
		return jsonMergeBadTarget
	}
	iTStart := iTarget + n
	iTEndBE := iTStart + sz

	for iPCursor < iPEnd {
		iPLabel := iPCursor
		ePLabel := pp.aBlob[iPCursor] & 0x0f
		if ePLabel < jsonbText || ePLabel > jsonbTextRaw {
			return jsonMergeBadPatch
		}
		nPLabel, szPLabel := pp.payloadSize(iPCursor)
		if nPLabel == 0 {
			return jsonMergeBadPatch
		}
		iPValue := iPCursor + nPLabel + szPLabel
		if iPValue >= iPEnd {
			return jsonMergeBadPatch
		}
		nPValue, szPValue := pp.payloadSize(iPValue)
		if nPValue == 0 {
			return jsonMergeBadPatch
		}
		iPCursor = iPValue + nPValue + szPValue
		if iPCursor > iPEnd {
			return jsonMergeBadPatch
		}

		iTCursor := iTStart
		iTEnd := iTEndBE + t.delta
		var iTLabel, nTLabel, szTLabel, nTValue, szTValue int
		for iTCursor < iTEnd {
			iTLabel = iTCursor
			eTLabel := t.aBlob[iTCursor] & 0x0f
			if eTLabel < jsonbText || eTLabel > jsonbTextRaw {
				return jsonMergeBadTarget
			}
			nTLabel, szTLabel = t.payloadSize(iTCursor)
			if nTLabel == 0 {
				return jsonMergeBadTarget
			}
			iTValue := iTLabel + nTLabel + szTLabel
			if iTValue >= iTEnd {
				return jsonMergeBadTarget
			}
			nTValue, szTValue = t.payloadSize(iTValue)
			if nTValue == 0 {
				return jsonMergeBadTarget
			}
			if iTValue+nTValue+szTValue > iTEnd {
				return jsonMergeBadTarget
			}
			if labelCompare(
				string(pp.aBlob[iPLabel+nPLabel:iPLabel+nPLabel+szPLabel]), ePLabel == jsonbText || ePLabel == jsonbTextRaw,
				string(t.aBlob[iTLabel+nTLabel:iTLabel+nTLabel+szTLabel]), eTLabel == jsonbText || eTLabel == jsonbTextRaw,
			) {
				break
			}
			iTCursor = iTValue + nTValue + szTValue
		}
		x := pp.aBlob[iPValue] & 0x0f
		if iTCursor < iTEnd {
			if x == 0 {
				// A null patch value removes the member.
				t.blobEdit(iTLabel, nTLabel+szTLabel+nTValue+szTValue, nil, 0)
			} else {
				savedDelta := t.delta
				t.delta = 0
				if iDepth >= jsonMaxDepth {
					return jsonMergeTooDeep
				}
				if rc := jsonMergePatch(t, iTLabel+nTLabel+szTLabel, pp, iPValue, iDepth+1); rc != 0 {
					return rc
				}
				t.delta += savedDelta
			}
		} else if x > 0 {
			szNew := szPLabel + nPLabel
			if pp.aBlob[iPValue]&0x0f != jsonbObject {
				t.blobEdit(iTEnd, 0, nil, szPValue+nPValue+szNew)
				copy(t.aBlob[iTEnd:], pp.aBlob[iPLabel:iPLabel+szNew])
				copy(t.aBlob[iTEnd+szNew:], pp.aBlob[iPValue:iPValue+szPValue+nPValue])
			} else {
				t.blobEdit(iTEnd, 0, nil, szNew+1)
				copy(t.aBlob[iTEnd:], pp.aBlob[iPLabel:iPLabel+szNew])
				t.aBlob[iTEnd+szNew] = 0x00
				savedDelta := t.delta
				t.delta = 0
				if iDepth >= jsonMaxDepth {
					return jsonMergeTooDeep
				}
				if rc := jsonMergePatch(t, iTEnd+szNew, pp, iPValue, iDepth+1); rc != 0 {
					return rc
				}
				t.delta += savedDelta
			}
		}
	}
	if t.delta != 0 {
		t.afterEditSizeAdjust(iTarget)
	}
	return jsonMergeOK
}

func jsonPatchFunc(a []any, flags int) (any, error) {
	target, err := jsonParseFuncArg(a[0], jsonEditable)
	if target == nil {
		return nil, err
	}
	patch, err := jsonParseFuncArg(a[1], 0)
	if patch == nil {
		return nil, err
	}
	switch jsonMergePatch(target, 0, patch, 0, 0) {
	case jsonMergeOK:
		return target.returnParse(flags&jsonFlagBlob != 0)
	case jsonMergeTooDeep:
		return nil, errJSONTooDeep
	}
	return nil, errMalformedJSON
}

func jsonObjectFunc(a []any, flags int) (any, error) {
	if len(a)&1 != 0 {
		return nil, errors.New("json_object() requires an even number of arguments")
	}
	var jx jsonString
	jx.appendChar('{')
	for i := 0; i < len(a); i += 2 {
		if c, _ := norm(a[i]); c != classText {
			return nil, errors.New("json_object() labels must be TEXT")
		}
		jx.appendSeparator()
		jx.appendString([]byte(sqlText(a[i])))
		jx.appendChar(':')
		jx.appendSqlValue(a[i+1])
	}
	jx.appendChar('}')
	return jx.result(flags&jsonFlagBlob != 0)
}

// jsonRemoveFunc is json_remove, and json and jsonb, which are json_remove with no paths.
func jsonRemoveFunc(a []any, flags int) (any, error) {
	if len(a) < 1 {
		return nil, nil
	}
	flgs := 0
	if len(a) > 1 {
		flgs = jsonEditable
	}
	p, err := jsonParseFuncArg(a[0], flgs)
	if p == nil {
		return nil, err
	}
	for i := 1; i < len(a); i++ {
		if isNull(a[i]) {
			return nil, nil
		}
		path := beforeNUL(sqlText(a[i]))
		if at(path, 0) != '$' {
			return nil, jsonBadPathError(path, 0)
		}
		if at(path, 1) == 0 {
			// json_remove(j, '$') is NULL.
			return nil, nil
		}
		p.eEdit = jeditDel
		p.delta = 0
		if rc := p.lookupStep(0, path, 1, 0); lookupIsError(rc) {
			if rc == lookupNotFound {
				continue
			}
			return nil, jsonBadPathError(path, rc)
		}
	}
	return p.returnParse(flags&jsonFlagBlob != 0)
}

func jsonWrongNumArgs(name string) error {
	return fmt.Errorf("json_%s() needs an odd number of arguments", name)
}

func jsonReplaceFunc(a []any, flags int) (any, error) {
	if len(a) < 1 {
		return nil, nil
	}
	if len(a)&1 == 0 {
		return nil, jsonWrongNumArgs("replace")
	}
	return jsonInsertIntoBlob(a, jeditRepl, flags)
}

func jsonSetFunc(a []any, flags int) (any, error) {
	insType := (flags & 0xc) >> 2
	if len(a) < 1 {
		return nil, nil
	}
	if len(a)&1 == 0 {
		return nil, jsonWrongNumArgs([]string{"insert", "set", "array_insert"}[insType])
	}
	return jsonInsertIntoBlob(a, []int{jeditIns, jeditSet, jeditAins}[insType], flags)
}

// jsonInsertIntoBlob applies each path and value pair to the first argument by eEdit.
func jsonInsertIntoBlob(a []any, eEdit int, flags int) (any, error) {
	flgs := 0
	if len(a) > 1 {
		flgs = jsonEditable
	}
	p, err := jsonParseFuncArg(a[0], flgs)
	if p == nil {
		return nil, err
	}
	for i := 1; i < len(a)-1; i += 2 {
		if isNull(a[i]) {
			continue
		}
		path := beforeNUL(sqlText(a[i]))
		if at(path, 0) != '$' {
			return nil, jsonBadPathError(path, 0)
		}
		ax, err := jsonFunctionArgToBlob(a[i+1])
		if err != nil {
			return nil, err
		}
		rc := 0
		if at(path, 1) == 0 {
			if eEdit == jeditRepl || eEdit == jeditSet {
				p.blobEdit(0, p.nBlob(), ax.aBlob, 0)
			}
		} else {
			p.eEdit = eEdit
			p.aIns = ax.aBlob
			p.delta = 0
			p.iDepth = 0
			rc = p.lookupStep(0, path, 1, 0)
		}
		if rc == lookupNotFound {
			continue
		}
		if lookupIsError(rc) {
			return nil, jsonBadPathError(path, rc)
		}
	}
	return p.returnParse(flags&jsonFlagBlob != 0)
}

func jsonTypeFunc(a []any, _ int) (any, error) {
	p, err := jsonParseFuncArg(a[0], 0)
	if p == nil {
		return nil, err
	}
	i := 0
	if len(a) == 2 {
		if isNull(a[1]) {
			return nil, nil
		}
		path := beforeNUL(sqlText(a[1]))
		if at(path, 0) != '$' {
			return nil, jsonBadPathError(path, 0)
		}
		i = p.lookupStep(0, path, 1, 0)
		if lookupIsError(i) {
			if i == lookupNotFound {
				return nil, nil
			}
			return nil, jsonBadPathError(path, i)
		}
	}
	return jsonbType[p.aBlob[i]&0x0f], nil
}

func jsonPrettyFunc(a []any, _ int) (any, error) {
	p, err := jsonParseFuncArg(a[0], 0)
	if p == nil {
		return nil, err
	}
	var s jsonString
	x := jsonPretty{p: p, out: &s, indent: "    "}
	if len(a) == 2 && !isNull(a[1]) {
		x.indent = beforeNUL(sqlText(a[1]))
	}
	x.translate(0)
	v, err := s.result(false)
	if t, ok := v.(jsonText); ok {
		return string(t), err // json_pretty's result carries no subtype
	}
	return v, err
}

func jsonValidFunc(a []any, _ int) (any, error) {
	flags := int64(1)
	if len(a) == 2 {
		f := sqlInt(a[1])
		if f < 1 || f > 15 {
			return nil, errors.New("FLAGS parameter to json_valid() must be between 1 and 15")
		}
		flags = f & 0x0f
	}
	switch c, _ := norm(a[0]); c {
	case classNull:
		return nil, nil
	case classBlob:
		if py, ok := jsonArgIsJsonb(a[0]); ok {
			switch {
			case flags&0x04 != 0:
				return int64(1), nil
			case flags&0x08 != 0:
				return boolInt(py.validityCheck(0, py.nBlob(), 1) == 0), nil
			}
			return int64(0), nil
		}
	}
	if flags&0x3 == 0 {
		return int64(0), nil
	}
	p, _ := jsonParseFuncArg(a[0], jsonKeepError)
	if p.nErr != 0 {
		return int64(0), nil
	}
	return boolInt(flags&0x02 != 0 || !p.hasNonstd), nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func jsonErrorFunc(a []any, _ int) (any, error) {
	if s, ok := jsonArgIsJsonb(a[0]); ok {
		return int64(s.validityCheck(0, s.nBlob(), 1)), nil
	}
	if isNull(a[0]) {
		return nil, nil
	}
	s := &jsonParse{zJson: sqlText(a[0]), hasJson: true}
	if s.convertTextToBlob() {
		return int64(0), nil
	}
	// The byte offset of the error as a character offset.
	var pos int64
	for k := 0; k < s.iErr && at(s.zJson, k) != 0; k++ {
		if s.zJson[k]&0xc0 != 0x80 {
			pos++
		}
	}
	return pos + 1, nil
}

// json_each and json_tree: a row per element, as json.c's virtual table cursor walks them.

var jsonEachColumns = []string{"key", "value", "type", "atom", "id", "parent", "fullkey", "path"}

type jsonParent struct {
	iHead, iValue, iEnd int
	nPath               int
	iKey                int64
}

type jsonEachCursor struct {
	i, iEnd   int
	iRowid    int
	nRoot     int
	eType     byte
	recursive bool
	eMode     int
	parents   []jsonParent
	path      []byte
	p         *jsonParse
}

func jsonEachTable(name string, eMode int, recursive bool) Func {
	return NewTable(name, jsonEachColumns, 1, 2, func(a []any) ([]map[string]any, error) {
		return sqliteJSONEach(a, eMode, recursive)
	})
}

func sqliteJSONEach(a []any, eMode int, recursive bool) ([]map[string]any, error) {
	cur := &jsonEachCursor{eMode: eMode, recursive: recursive}
	if p, ok := jsonArgIsJsonb(a[0]); ok {
		cur.p = p
	} else {
		if isNull(a[0]) {
			return nil, nil
		}
		cur.p = &jsonParse{zJson: sqlText(a[0]), hasJson: true}
		if !cur.p.convertTextToBlob() {
			return nil, errMalformedJSON
		}
	}
	p := cur.p
	var i int
	if len(a) == 2 {
		if isNull(a[1]) {
			return nil, nil
		}
		root := beforeNUL(sqlText(a[1]))
		if at(root, 0) != '$' {
			return nil, jsonBadPathError(root, 0)
		}
		cur.nRoot = len(root)
		if at(root, 1) == 0 {
			i, cur.i = 0, 0
			cur.eType = 0
		} else {
			i = p.lookupStep(0, root, 1, 0)
			if lookupIsError(i) {
				if i == lookupNotFound {
					return nil, nil
				}
				return nil, jsonBadPathError(root, 0)
			}
			if p.iLabel != 0 {
				cur.i = p.iLabel
				cur.eType = jsonbObject
			} else {
				cur.i = i
				cur.eType = jsonbArray
			}
		}
		cur.path = append(cur.path, root...)
	} else {
		i, cur.i = 0, 0
		cur.eType = 0
		cur.nRoot = 1
		cur.path = append(cur.path, '$')
	}
	n, sz := p.payloadSize(i)
	cur.iEnd = i + n + sz
	if p.aBlob[i]&0x0f >= jsonbArray && !recursive {
		cur.i = i + n
		cur.eType = p.aBlob[i] & 0x0f
		cur.parents = []jsonParent{{iKey: 0, iEnd: cur.iEnd, iHead: cur.i, iValue: i}}
	}
	var rows []map[string]any
	for cur.i < cur.iEnd {
		row := make(map[string]any, len(jsonEachColumns))
		for k, col := range jsonEachColumns {
			v, err := cur.column(k)
			if err != nil {
				return nil, err
			}
			row[col] = v
		}
		rows = append(rows, row)
		cur.next()
	}
	return rows, nil
}

// skipLabel is the index of the current value: past the label when the container is an object.
func (c *jsonEachCursor) skipLabel() int {
	if c.eType == jsonbObject {
		n, sz := c.p.payloadSize(c.i)
		sz += c.i + n
		if sz >= c.p.nBlob() {
			sz = c.i
		}
		return sz
	}
	return c.i
}

// appendPathName appends the current element's step to the path: [N] or .label, quoted unless the
// label is alphanumeric starting with a letter.
func (c *jsonEachCursor) appendPathName() {
	if c.eType == jsonbArray {
		c.path = append(c.path, fmt.Sprintf("[%d]", c.parents[len(c.parents)-1].iKey)...)
		return
	}
	n, sz := c.p.payloadSize(c.i)
	z := c.p.aBlob[c.i+n : c.i+n+sz]
	needQuote := sz == 0 || !sqlIsAlpha(z[0])
	for k := 0; !needQuote && k < sz; k++ {
		if !sqlIsAlnum(z[k]) {
			needQuote = true
		}
	}
	label := beforeNUL(string(z)) // %.*s stops at a NUL
	if needQuote {
		c.path = append(c.path, ".\""+label+"\""...)
	} else {
		c.path = append(c.path, "."+label...)
	}
}

func (c *jsonEachCursor) next() {
	p := c.p
	if c.recursive {
		levelChange := false
		i := c.skipLabel()
		x := p.aBlob[i] & 0x0f
		n, sz := p.payloadSize(i)
		if x == jsonbObject || x == jsonbArray {
			levelChange = true
			parent := jsonParent{iHead: c.i, iValue: i, iEnd: i + n + sz, iKey: -1, nPath: len(c.path)}
			if c.eType != 0 && len(c.parents) > 0 {
				c.appendPathName()
			}
			c.parents = append(c.parents, parent)
			c.i = i + n
		} else {
			c.i = i + n + sz
		}
		for len(c.parents) > 0 && c.i >= c.parents[len(c.parents)-1].iEnd {
			last := c.parents[len(c.parents)-1]
			c.parents = c.parents[:len(c.parents)-1]
			c.path = c.path[:last.nPath]
			levelChange = true
		}
		if levelChange {
			if len(c.parents) > 0 {
				c.eType = p.aBlob[c.parents[len(c.parents)-1].iValue] & 0x0f
			} else {
				c.eType = 0
			}
		}
	} else {
		i := c.skipLabel()
		n, sz := p.payloadSize(i)
		c.i = i + n + sz
	}
	if c.eType == jsonbArray && len(c.parents) > 0 {
		c.parents[len(c.parents)-1].iKey++
	}
	c.iRowid++
}

// pathLength is the length of the path for the first row of json_tree, which is the root's parent
// path.
func (c *jsonEachCursor) pathLength() int {
	n := len(c.path)
	z := c.path
	if c.iRowid == 0 && c.recursive && n >= 2 {
		for n > 1 {
			n--
			if z[n] == '[' || z[n] == '.' {
				x := c.p.lookupStep(0, string(z[:n]), 1, 0)
				if lookupIsError(x) {
					continue
				}
				if k, _ := c.p.payloadSize(x); x+k == c.i {
					break
				}
			}
		}
	}
	return n
}

func (c *jsonEachCursor) column(col int) (any, error) {
	p := c.p
	switch col {
	case 0: // key
		if len(c.parents) == 0 {
			if c.nRoot == 1 {
				return nil, nil
			}
			j := c.pathLength()
			n := c.nRoot - j
			switch {
			case n == 0:
				return nil, nil
			case c.path[j] == '[':
				x, _ := atoi64(string(c.path[j+1 : j+n]))
				return x, nil
			case c.path[j+1] == '"':
				return string(c.path[j+2 : j+2+n-3]), nil
			}
			return string(c.path[j+1 : j+n]), nil
		}
		if c.eType == jsonbObject {
			return p.returnFromBlob(c.i, 1)
		}
		return c.parents[len(c.parents)-1].iKey, nil
	case 1: // value
		i := c.skipLabel()
		v, err := p.returnFromBlob(i, c.eMode)
		if err != nil {
			return nil, err
		}
		if s, ok := v.(jsonText); ok && p.aBlob[i]&0x0f < jsonbArray {
			return string(s), nil
		}
		return v, nil
	case 2: // type
		return jsonbType[p.aBlob[c.skipLabel()]&0x0f], nil
	case 3: // atom
		i := c.skipLabel()
		if p.aBlob[i]&0x0f < jsonbArray {
			return p.returnFromBlob(i, 1)
		}
		return nil, nil
	case 4: // id
		return int64(c.i), nil
	case 5: // parent
		if len(c.parents) > 0 && c.recursive {
			return int64(c.parents[len(c.parents)-1].iHead), nil
		}
		return nil, nil
	case 6: // fullkey
		base := len(c.path)
		if len(c.parents) > 0 {
			c.appendPathName()
		}
		s := string(c.path)
		c.path = c.path[:base]
		return s, nil
	case 7: // path
		return string(c.path[:c.pathLength()]), nil
	}
	return nil, nil
}
