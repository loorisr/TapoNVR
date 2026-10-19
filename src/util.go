package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------- util: structured logging -----------------

type Lvl int

const (
	LDEBUG Lvl = 0
	LINFO  Lvl = 1
	LWARN  Lvl = 2
	LERR   Lvl = 3
)

const Reset = "\x1b[0m"
const Yellow = "\x1b[33m"
const Blue = "\x1b[34m"
const Cyan = "\x1b[36m"
const Red = "\x1b[31m"

var logMtx sync.Mutex
var logMin Lvl = LINFO

func logName(l Lvl) string {
	if l == LDEBUG {
		return "DEBUG"
	}
	if l == LINFO {
		return "INFO"
	}
	if l == LWARN {
		return "WARN"
	}
	return "ERROR"
}

func logColor(l Lvl) string {
	if l == LDEBUG {
		return Cyan
	}
	if l == LINFO {
		return Blue
	}
	if l == LWARN {
		return Yellow
	}
	return Red
}

// jsonString renders a JSON string literal. Every control byte below 0x20 is
// emitted as \u00XX: leaving one raw produces invalid JSON in the API, SSE and
// MQTT payloads. Bytes >= 0x80 pass through untouched (UTF-8 is valid JSON).
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		var c = s[i]
		switch c {
		case '\\':
			b.WriteString("\\\\")
		case '"':
			b.WriteString("\\\"")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if c < 0x20 {
				b.WriteString(fmt.Sprintf("\\u%04x", c))
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func kv(key string, val string) string { return jsonString(key) + ":" + jsonString(val) }

// sanitizeBboxJSON validates stored bbox JSON for embedding in API/SSE/MQTT
// payloads. A single corrupt row must not break the whole response, so
// invalid JSON reports ok=false and callers fall back to null/omission.
func sanitizeBboxJSON(s string) (string, bool) {
	var t = strings.TrimSpace(s)
	if t == "" {
		return "", false
	}
	var v any
	if json.Unmarshal([]byte(t), &v) != nil {
		return "", false
	}
	return t, true
}

// kvRaw embeds an already-encoded JSON value (bool/number/object) without quoting.
func kvRaw(key string, raw string) string { return jsonString(key) + ":" + raw }

func logIt(l Lvl, msg string, kvs map[string]string) {
	if l < logMin {
		return
	}
	var parts []string
	parts = append(parts, "\"ts\":\""+time.Now().UTC().Format("2006-01-02T15:04:05.000Z")+"\"")
	parts = append(parts, "\"level\":\""+logName(l)+"\"")
	parts = append(parts, kv("msg", msg))
	// Deterministic field order (map iteration is random): sort keys so log
	// lines grep/diff cleanly.
	var keys = make([]string, 0, len(kvs))
	for k := range kvs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, kv(k, kvs[k]))
	}
	var line = "{" + strings.Join(parts, ",") + "}"
	logMtx.Lock()
	defer logMtx.Unlock()
	_, _ = io.WriteString(os.Stdout, logColor(l)+line+Reset+"\n")
}

// escapeJSON escapes a value for embedding inside an already-built JSON string
// literal (backslashes first, or quote escaping would double-escape them).
// Control bytes are removed so the surrounding literal stays valid JSON.
func escapeJSON(s string) string {
	var full = jsonString(s)
	if len(full) >= 2 {
		return full[1 : len(full)-1]
	}
	return ""
}

// truncateForLog caps a string for log fields (Pushover rejections, etc.).
// Newlines/carriage returns are flattened so one event stays one log line.
// Truncation respects UTF-8 rune boundaries so multi-byte messages stay valid.
func truncateForLog(s string, max int) string {
	if max <= 0 {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	// Cut at the last rune boundary at or before max so multi-byte runes
	// (e.g. accented Pushover messages) stay valid UTF-8.
	var cut = 0
	for i := range s {
		if i > max {
			break
		}
		cut = i
	}
	return s[:cut]
}
