package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// foldKey folds a key exactly as encoding/json does when it matches keys
// to fields (ASCII upper case; other runes to the smallest rune of their
// case-folding orbit, so that "ſ" is "S" and the Kelvin sign is "K").
// Two keys that fold alike are ones Go's decoder could confuse.
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf:
			if 'a' <= r && r <= 'z' {
				r -= 'a' - 'A'
			}
		default:
			for {
				r2 := unicode.SimpleFold(r)
				if r2 <= r {
					r = r2
					break
				}
				r = r2
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// maxDepth bounds the nesting StrictJSON accepts.
const maxDepth = 64

// StrictJSON accepts only JSON every parser reads the same way: valid
// UTF-8 without a byte order mark, one value, and no object with two keys
// that are equal or equal but for case. Go's decoder matches keys without
// regard to case and keeps the last of two equal ones; others keep the
// first, or keep cases apart. A message that the gate and the server
// could read differently is refused, so the server never acts on
// something the gate did not see.
func StrictJSON(b []byte) error {
	if !utf8.Valid(b) {
		return errors.New("not valid UTF-8")
	}
	if bytes.HasPrefix(b, []byte("\xef\xbb\xbf")) {
		return errors.New("starts with a byte order mark")
	}
	if !json.Valid(b) {
		return errors.New("not valid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	// Each open container: for objects, the keys seen (lower case) and
	// whether a key is expected next.
	type level struct {
		object    bool
		keys      map[string]bool
		expectKey bool
	}
	var stack []*level
	values := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("not valid JSON: %w", err)
		}
		var top *level
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.expectKey {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].expectKey = true
				}
				continue
			}
			key, ok := tok.(string)
			if !ok {
				return errors.New("not valid JSON: an object key that is not a string")
			}
			k := foldKey(key)
			if top.keys[k] {
				return fmt.Errorf("the key %q appears twice (perhaps in another case)", key)
			}
			top.keys[k] = true
			top.expectKey = false
			continue
		}
		if len(stack) == 0 {
			if values++; values > 1 {
				return errors.New("more than one JSON value")
			}
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if len(stack) >= maxDepth {
					return fmt.Errorf("nested deeper than %d", maxDepth)
				}
				stack = append(stack, &level{object: d == '{', keys: map[string]bool{}, expectKey: d == '{'})
				continue
			case ']':
				stack = stack[:len(stack)-1]
			}
		}
		// A value ended: an enclosing object expects a key next.
		if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].expectKey = true
		}
	}
	if values != 1 || len(stack) != 0 {
		return errors.New("not one complete JSON value")
	}
	return nil
}
