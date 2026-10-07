package main

import (
	"bytes"
	"encoding/json"
	"regexp"
)

var (
	// a US SSN, dashed
	ssn = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	// an account or card number: a run of 8 to 17 digits standing alone
	account = regexp.MustCompile(`\b\d{8,17}\b`)
)

// maskString masks every SSN and account number in s, keeping the last four
// digits ("•••-••-6789", "••••1234"), and returns how many it masked.
func maskString(s string) (string, int) {
	n := 0
	s = ssn.ReplaceAllStringFunc(s, func(m string) string {
		n++
		return "•••-••-" + m[len(m)-4:]
	})
	s = account.ReplaceAllStringFunc(s, func(m string) string {
		n++
		return "••••" + m[len(m)-4:]
	})
	return s, n
}

// maskValue masks every string in v, at any depth. Numbers, booleans and
// object keys are left as they are.
func maskValue(v any) (any, int) {
	switch t := v.(type) {
	case string:
		return maskString(t)
	case map[string]any:
		n := 0
		for k, e := range t {
			var c int
			t[k], c = maskValue(e)
			n += c
		}
		return t, n
	case []any:
		n := 0
		for i, e := range t {
			var c int
			t[i], c = maskValue(e)
			n += c
		}
		return t, n
	}
	return v, 0
}

// maskToolResult masks a tools/call result (MCP CallToolResult, raw JSON):
// the text of each content item and everything in structuredContent. It
// returns the masked result and how many it masked; with none, the result is
// nil and the caller passes the original through untouched.
func maskToolResult(raw []byte) ([]byte, int, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // numbers go back out exactly as they came
	var r map[string]any
	if err := dec.Decode(&r); err != nil {
		return nil, 0, err
	}
	n := 0
	if content, ok := r["content"].([]any); ok {
		for _, item := range content {
			if c, ok := item.(map[string]any); ok {
				if text, ok := c["text"].(string); ok {
					var k int
					c["text"], k = maskString(text)
					n += k
				}
			}
		}
	}
	if sc, ok := r["structuredContent"]; ok {
		var k int
		r["structuredContent"], k = maskValue(sc)
		n += k
	}
	if n == 0 {
		return nil, 0, nil
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, 0, err
	}
	return bytes.TrimRight(out.Bytes(), "\n"), n, nil
}
