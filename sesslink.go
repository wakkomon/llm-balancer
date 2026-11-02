package main

import (
	"encoding/json"
	"hash/fnv"
	"regexp"
	"strings"
	"time"
)

// Linking a session across context compression: agents like Hermes replace old messages with a summary
// when the context gets too long. The first user message – part of the fingerprint – is then gone, so the
// fingerprint changes. The recent messages are usually kept, though: if a new fingerprint shows up whose
// messages overlap with what an existing session of the same API token and system prompt recently sent or
// received, the new fingerprint is treated as the same session.

const (
	linkMinChars = 40            // shorter messages ("ok", "weiter") don't count
	linkMaxKeys  = 400           // remembered message keys per session
	linkWindow   = 3 * time.Hour // only sessions active this recently are candidates
)

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>|^\s*<think>.*$`)
var spaceRe = regexp.MustCompile(`\s+`)

// msgKey: a hash of the normalized start of a message (0 = too short to count).
func msgKey(text string) uint64 {
	t := thinkRe.ReplaceAllString(text, "")
	t = strings.TrimSpace(spaceRe.ReplaceAllString(systemReminderRe.ReplaceAllString(t, ""), " "))
	if len([]rune(t)) < linkMinChars {
		return 0
	}
	if r := []rune(t); len(r) > 300 {
		t = string(r[:300])
	}
	h := fnv.New64a()
	h.Write([]byte(t))
	return h.Sum64()
}

// requestKeys: keys of the non-system messages of a request, and a key of its system prompt.
func requestKeys(body []byte) (keys []uint64, sys uint64) {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return nil, 0
	}
	sysText := contentText(doc["system"])
	msgs, _ := doc["messages"].([]any)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		text := contentText(mm["content"])
		if role == "system" || role == "developer" {
			if sysText == "" {
				sysText = text
			}
			continue
		}
		if k := msgKey(text); k != 0 {
			keys = append(keys, k)
		}
	}
	h := fnv.New64a()
	h.Write([]byte(strings.TrimSpace(sysText)))
	return keys, h.Sum64()
}

type sessLink struct {
	token  string
	sys    uint64
	keys   map[uint64]time.Time
	last   time.Time
	answer uint64 // key of the latest generated answer
}

func (l *sessLink) add(k uint64, now time.Time) {
	if k == 0 {
		return
	}
	l.keys[k] = now
	if len(l.keys) > linkMaxKeys { // forget the oldest
		var oldK uint64
		var oldT time.Time
		for kk, t := range l.keys {
			if oldT.IsZero() || t.Before(oldT) {
				oldK, oldT = kk, t
			}
		}
		delete(l.keys, oldK)
	}
}

// resolve maps a detected session ID to the session it continues (fingerprints only). Returns the ID to use
// and, if it was linked now or earlier, the original fingerprint ID.
func (s *sessionStore) resolve(id, src, token string, body []byte) (string, string) {
	if id == "" {
		return id, ""
	}
	keys, sys := requestKeys(body)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.links == nil {
		s.links, s.alias = map[string]*sessLink{}, map[string]string{}
	}
	from := ""
	if a, ok := s.alias[id]; ok {
		from, id = id, a
	} else if strings.HasPrefix(src, "Fingerprint") && s.links[id] == nil && len(keys) >= 2 {
		// a fresh conversation has one or two messages; a compressed one keeps a summary plus recent turns.
		// Messages that several sessions share (injected boilerplate) don't count.
		shared := map[uint64]int{}
		for _, l := range s.links {
			for _, k := range keys {
				if _, ok := l.keys[k]; ok {
					shared[k]++
				}
			}
		}
		best, bestN, bestAns := "", 0, false
		for sid, l := range s.links {
			if l.token != token || l.sys != sys || now.Sub(l.last) > linkWindow {
				continue
			}
			n, ans := 0, false
			for _, k := range keys {
				if _, ok := l.keys[k]; ok && shared[k] == 1 {
					n++
					ans = ans || k == l.answer
				}
			}
			if n > bestN || (n == bestN && n > 0 && l.last.After(s.links[best].last)) {
				best, bestN, bestAns = sid, n, ans
			}
		}
		if bestN >= 2 || (bestN == 1 && bestAns) {
			s.alias[id] = best
			from, id = id, best
		}
	}
	l := s.links[id]
	if l == nil {
		l = &sessLink{token: token, sys: sys, keys: map[uint64]time.Time{}}
		s.links[id] = l
		if len(s.links) > maxSessionInfos*2 {
			s.pruneLinksLocked(now)
		}
	}
	l.last = now
	for _, k := range keys {
		l.add(k, now)
	}
	return id, from
}

// noteAnswer remembers the start of a generated answer – the next request (maybe already compressed)
// carries it as an assistant message.
func (s *sessionStore) noteAnswer(id, text string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if si := s.m[id]; si != nil {
		if t := strings.TrimSpace(thinkRe.ReplaceAllString(text, "")); t != "" {
			si.LastAnswer = clip(t, 400)
		}
	}
	k := msgKey(text)
	if k == 0 {
		return
	}
	if l := s.links[id]; l != nil {
		l.add(k, time.Now())
		l.answer = k
	}
}

func (s *sessionStore) pruneLinksLocked(now time.Time) {
	for id, l := range s.links {
		if now.Sub(l.last) > linkWindow {
			delete(s.links, id)
		}
	}
	for a, id := range s.alias {
		if s.links[id] == nil {
			delete(s.alias, a)
		}
	}
}
