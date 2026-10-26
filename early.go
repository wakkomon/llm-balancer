package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Some servers answer HTTP 200 and only then report the failure in the body: llama.cpp sends an SSE
// event {"error":…}, Ollama a line {"error":"…"}, sometimes the body is just empty. Such a response
// is only checked here, before anything is sent to the client, so another server can still be tried.

const peekLimit = 256 << 10

type peekVerdict int

const (
	peekMore peekVerdict = iota // need more data
	peekOK                      // real content (or nothing we can judge): start streaming
	peekBad                     // the response is an error / empty: try another server
)

// peekStart reads the beginning of a 2xx response until it can tell a real answer from an error.
// It returns the bytes read (to be sent on), a failure description (bad != "") or a read error.
func peekStart(resp *http.Response, onFirstByte func()) (buf []byte, bad string, err error) {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	sse := strings.Contains(ct, "event-stream")
	if !sse && !strings.Contains(ct, "json") {
		return nil, "", nil // text, audio, …: not ours to judge
	}
	tmp := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			if len(buf) == 0 && onFirstByte != nil {
				onFirstByte()
			}
			buf = append(buf, tmp[:n]...)
		}
		eof := rerr == io.EOF
		var v peekVerdict
		var msg string
		if sse {
			v, msg = inspectSSE(buf, eof)
		} else {
			v, msg = inspectJSON(buf, eof)
		}
		switch {
		case v == peekOK:
			return buf, "", nil
		case v == peekBad:
			return buf, msg, nil
		case rerr != nil && !eof:
			return buf, "", rerr
		case eof:
			return buf, "", nil
		case len(buf) >= peekLimit:
			return buf, "", nil // a lot of data without a verdict: let it through
		}
	}
}

// inspectSSE looks at the first complete events: keep-alive comments are skipped, an error event or
// an end without any content is bad, anything else is a real answer.
func inspectSSE(buf []byte, eof bool) (peekVerdict, string) {
	text := strings.ReplaceAll(string(buf), "\r\n", "\n")
	events := strings.Split(text, "\n\n")
	complete := events[:len(events)-1]
	if eof {
		complete = events
	}
	for _, ev := range complete {
		var data []string
		for _, line := range strings.Split(ev, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(data) == 0 {
			continue // comment / keep-alive / event name only
		}
		payload := strings.Join(data, "\n")
		if payload == "[DONE]" {
			return peekBad, "leere Antwort (Stream ohne Inhalt beendet)"
		}
		if msg, isErr := errorObject([]byte(payload)); isErr {
			return peekBad, "Fehler im Stream: " + msg
		}
		return peekOK, ""
	}
	if eof {
		return peekBad, "leere Antwort"
	}
	return peekMore, ""
}

// inspectJSON handles a plain JSON body or NDJSON (Ollama): the first complete line, or at the end
// the whole body.
func inspectJSON(buf []byte, eof bool) (peekVerdict, string) {
	if i := bytes.IndexByte(buf, '\n'); i >= 0 {
		line := bytes.TrimSpace(buf[:i])
		if len(line) > 0 && json.Valid(line) {
			if msg, isErr := errorObject(line); isErr {
				return peekBad, "Fehler in der Antwort: " + msg
			}
			return peekOK, ""
		}
	}
	if !eof {
		return peekMore, ""
	}
	body := bytes.TrimSpace(buf)
	if len(body) == 0 {
		return peekBad, "leere Antwort"
	}
	if msg, isErr := errorObject(body); isErr {
		return peekBad, "Fehler in der Antwort: " + msg
	}
	return peekOK, ""
}

// errorObject: is this JSON an error report without any answer in it?
func errorObject(b []byte) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return "", false
	}
	e, ok := m["error"]
	if !ok || string(e) == "null" || string(e) == `""` {
		return "", false
	}
	for _, k := range []string{"choices", "message", "response", "content", "delta"} {
		if v, ok := m[k]; ok && string(v) != "null" {
			return "", false
		}
	}
	return errorText(e), true
}

func errorText(e json.RawMessage) string {
	var s string
	if json.Unmarshal(e, &s) == nil {
		return s
	}
	var o struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	}
	if json.Unmarshal(e, &o) == nil && o.Message != "" {
		if o.Code != nil {
			return fmt.Sprintf("%s (%v)", o.Message, o.Code)
		}
		return o.Message
	}
	return shortBody(e)
}
