package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Request bodies of agents are large (hundreds of KB up to several MB: the whole history, tool output,
// images). They are decoded once per request (parseDoc) and every consumer works on that document;
// the body itself is only patched byte-wise (model name, stream_options) and otherwise forwarded as is.

// parseDoc decodes a JSON object body; nil for anything else.
func parseDoc(body []byte) map[string]any {
	b := bytes.TrimLeft(body, " \t\r\n")
	if len(b) == 0 || b[0] != '{' {
		return nil
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	return doc
}

func docString(doc map[string]any, key string) string {
	s, _ := doc[key].(string)
	return s
}

// stripBlocks removes <open>…</close> blocks (non-greedy, like the former regexps) – without a regexp
// scan when the text does not contain the tag at all, which is the normal case.
func stripBlocks(s, open, close string) string {
	if !strings.Contains(s, open) {
		return s
	}
	var sb strings.Builder
	for {
		i := strings.Index(s, open)
		if i < 0 {
			break
		}
		j := strings.Index(s[i+len(open):], close)
		if j < 0 {
			break
		}
		sb.WriteString(s[:i])
		s = s[i+len(open)+j+len(close):]
	}
	sb.WriteString(s)
	return sb.String()
}

func stripReminders(s string) string {
	return stripBlocks(s, "<system-reminder>", "</system-reminder>")
}

// normPrefix: whitespace collapsed to single spaces, trimmed, at most n runes – reading only as much
// of s as needed.
func normPrefix(s string, n int) (string, int) {
	var sb strings.Builder
	runes, space := 0, false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' {
			space = runes > 0
			continue
		}
		if space {
			sb.WriteByte(' ')
			runes++
			space = false
			if runes >= n {
				break
			}
		}
		sb.WriteRune(r)
		runes++
		if runes >= n {
			break
		}
	}
	return sb.String(), runes
}

// ---- byte-level patches of the top-level object ----

// skipValue returns the index after the JSON value starting at b[i] (whitespace before it is skipped).
func skipValue(b []byte, i int) int {
	i = skipWS(b, i)
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = skipString(b, i)
				if i < 0 {
					return -1
				}
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return -1
	default: // number, true, false, null
		for i < len(b) && !bytes.ContainsRune([]byte(",}] \t\r\n"), rune(b[i])) {
			i++
		}
		return i
	}
}

func skipWS(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	return i
}

// skipString: b[i] is the opening quote; returns the index after the closing quote.
func skipString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return -1
}

// topLevelValue finds the value of a top-level key: its start and end offsets. found=false if the
// key is missing; ok=false if the body could not be scanned.
func topLevelValue(b []byte, key string) (start, end int, found, ok bool) {
	i := skipWS(b, 0)
	if i >= len(b) || b[i] != '{' {
		return 0, 0, false, false
	}
	i++
	for {
		i = skipWS(b, i)
		if i < len(b) && b[i] == '}' {
			return 0, 0, false, true
		}
		if i >= len(b) || b[i] != '"' {
			return 0, 0, false, false
		}
		ks := i
		i = skipString(b, i)
		if i < 0 {
			return 0, 0, false, false
		}
		var k string
		if json.Unmarshal(b[ks:i], &k) != nil {
			return 0, 0, false, false
		}
		i = skipWS(b, i)
		if i >= len(b) || b[i] != ':' {
			return 0, 0, false, false
		}
		vs := skipWS(b, i+1)
		ve := skipValue(b, vs)
		if ve < 0 {
			return 0, 0, false, false
		}
		if k == key {
			return vs, ve, true, true
		}
		i = skipWS(b, ve)
		if i < len(b) && b[i] == ',' {
			i++
			continue
		}
		if i < len(b) && b[i] == '}' {
			return 0, 0, false, true
		}
		return 0, 0, false, false
	}
}

// withModel sets the top-level "model" field; the rest of the body stays byte for byte.
func withModel(body []byte, model string) []byte {
	mb, _ := json.Marshal(model)
	s, e, found, ok := topLevelValue(body, "model")
	if !ok {
		return body
	}
	if !found {
		i := bytes.IndexByte(body, '{')
		rest := bytes.TrimLeft(body[i+1:], " \t\r\n")
		out := make([]byte, 0, len(body)+len(mb)+12)
		out = append(out, body[:i+1]...)
		out = append(out, `"model":`...)
		out = append(out, mb...)
		if len(rest) > 0 && rest[0] != '}' {
			out = append(out, ',')
		}
		return append(out, body[i+1:]...)
	}
	out := make([]byte, 0, len(body)-(e-s)+len(mb))
	out = append(out, body[:s]...)
	out = append(out, mb...)
	return append(out, body[e:]...)
}

// withStreamUsage adds stream_options.include_usage=true to an OpenAI-style streaming request that
// doesn't ask for it, so the final chunk reports token counts. Other fields stay untouched.
func withStreamUsage(body []byte) ([]byte, bool) {
	_, _, found, ok := topLevelValue(body, "stream_options")
	if !ok || found {
		return body, false
	}
	i := bytes.IndexByte(body, '{')
	rest := bytes.TrimLeft(body[i+1:], " \t\r\n")
	out := make([]byte, 0, len(body)+40)
	out = append(out, body[:i+1]...)
	out = append(out, `"stream_options":{"include_usage":true}`...)
	if len(rest) > 0 && rest[0] != '}' {
		out = append(out, ',')
	}
	return append(out, body[i+1:]...), true
}
