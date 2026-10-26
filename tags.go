package main

import (
	"fmt"
	"strings"
)

// DefaultTagCheck are the tags LLMs commonly emit in their text output (reasoning, tool calls).
// Arbitrary HTML in answers is ignored unless TAG_CHECK=* is set.
const DefaultTagCheck = "think,thinking,reasoning,tool_call,tool_calls,function_call,answer"

// implicitOpen: chat templates of reasoning models (Qwen3, DeepSeek-R1, …) put "<think>" into the
// prompt, so the output starts with the reasoning and only contains the closing tag.
var implicitOpen = map[string]bool{"think": true, "thinking": true, "reasoning": true}

const maxTagIssues = 5

// tagChecker follows the generated text and reports tags that are not opened/closed correctly.
type tagChecker struct {
	names   map[string]bool // nil = every tag
	stack   []string
	issues  []string
	carry   string // incomplete "<…" at the end of the last piece
	seenTag bool
}

func newTagChecker(spec string) *tagChecker {
	spec = strings.TrimSpace(strings.ToLower(spec))
	if spec == "" || spec == "off" || spec == "false" || spec == "0" {
		return nil
	}
	t := &tagChecker{}
	if spec != "*" {
		t.names = map[string]bool{}
		for _, n := range strings.Split(spec, ",") {
			if n = strings.TrimSpace(n); n != "" {
				t.names[n] = true
			}
		}
	}
	return t
}

func (t *tagChecker) issue(format string, a ...any) {
	if len(t.issues) < maxTagIssues {
		t.issues = append(t.issues, fmt.Sprintf(format, a...))
	}
}

func isTagStart(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' }
func isTagChar(c byte) bool {
	return isTagStart(c) || c >= '0' && c <= '9' || c == '-' || c == ':' || c == '.'
}

// Feed processes the next piece of generated text. Tags may be split across pieces.
func (t *tagChecker) Feed(s string) {
	if t == nil || s == "" {
		return
	}
	buf := t.carry + s
	t.carry = ""
	for {
		i := strings.IndexByte(buf, '<')
		if i < 0 {
			return
		}
		buf = buf[i:]
		end := strings.IndexByte(buf, '>')
		if end < 0 {
			if len(buf) < 256 { // possibly a tag that continues in the next piece
				t.carry = buf
			}
			return
		}
		t.tag(buf[1:end])
		buf = buf[end+1:]
	}
}

func (t *tagChecker) tag(inner string) {
	closing := strings.HasPrefix(inner, "/")
	if closing {
		inner = inner[1:]
	}
	if inner == "" || !isTagStart(inner[0]) || strings.HasSuffix(inner, "/") {
		return // "<3", "< x", comments, self-closing tags …
	}
	n := 1
	for n < len(inner) && isTagChar(inner[n]) {
		n++
	}
	if n < len(inner) && inner[n] != ' ' && inner[n] != '\t' && inner[n] != '\n' {
		return // "a<b>c" in code etc.: not a plain tag
	}
	name := strings.ToLower(inner[:n])
	if t.names != nil && !t.names[name] {
		return
	}
	first := !t.seenTag
	t.seenTag = true
	if !closing {
		for _, open := range t.stack {
			if open == name {
				t.issue("<%s> erneut geöffnet, obwohl noch offen", name)
				break
			}
		}
		t.stack = append(t.stack, name)
		return
	}
	if k := len(t.stack) - 1; k >= 0 && t.stack[k] == name {
		t.stack = t.stack[:k]
		return
	}
	for k := len(t.stack) - 1; k >= 0; k-- {
		if t.stack[k] == name {
			t.issue("</%s> schließt, aber <%s> ist noch offen", name, t.stack[len(t.stack)-1])
			t.stack = t.stack[:k]
			return
		}
	}
	if first && len(t.stack) == 0 && implicitOpen[name] {
		return // opened by the chat template
	}
	t.issue("</%s> ohne öffnendes <%s>", name, name)
}

// Finish reports tags that are still open and returns all issues.
func (t *tagChecker) Finish(complete bool) []string {
	if t == nil {
		return nil
	}
	if complete {
		for k := len(t.stack) - 1; k >= 0; k-- {
			t.issue("<%s> nicht geschlossen", t.stack[k])
		}
	}
	return t.issues
}
