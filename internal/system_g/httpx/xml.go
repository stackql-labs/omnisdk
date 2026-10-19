package httpx

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"slices"
)

// xmlDoc writes a document as XML elements. Keys are written in sorted order, so the same document is
// always the same bytes; a key with no value is left out.
func xmlDoc(doc map[string]any) []byte {
	var b bytes.Buffer
	for _, k := range sortedKeys(doc) {
		xmlElem(&b, k, doc[k])
	}
	return b.Bytes()
}

func xmlElem(b *bytes.Buffer, name string, v any) {
	switch x := v.(type) {
	case nil:
	case []any:
		for _, e := range x {
			xmlElem(b, name, e)
		}
	case map[string]any:
		fmt.Fprintf(b, "<%s>", name)
		for _, k := range sortedKeys(x) {
			xmlElem(b, k, x[k])
		}
		fmt.Fprintf(b, "</%s>", name)
	default:
		fmt.Fprintf(b, "<%s>", name)
		_ = xml.EscapeText(b, []byte(fmt.Sprint(x)))
		fmt.Fprintf(b, "</%s>", name)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
