// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package assessment

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxJSONBytes bounds each rubric or scoring document, including nested data.
const MaxJSONBytes = 16_384

// Unlike encoding/json's ordinary object decoding, this reader rejects
// duplicate keys (including escaped equivalents) instead of keeping the last.
// A signed or independently reviewed payload must have only one interpretation.
func parseRubricSchemaJSON(field, raw string) (any, error) {
	if len(raw) > MaxJSONBytes || !utf8.ValidString(raw) {
		return nil, fmt.Errorf("%s must be UTF-8 JSON within %d bytes", field, MaxJSONBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	parsed, err := readJSONValue(decoder, 0)
	if err != nil {
		return nil, fmt.Errorf("%s must be unambiguous JSON: %w", field, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("%s must contain a single JSON value", field)
	}
	return parsed, nil
}

func readJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, fmt.Errorf("JSON nesting exceeds 32 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object key must be a string")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("duplicate object key %q", key)
			}
			value, err := readJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return object, nil
	case '[':
		values := make([]any, 0)
		for decoder.More() {
			value, err := readJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		return values, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter")
	}
}

func rubricFiniteNumber(value any) (float64, bool, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Float64()
		return n, false, err == nil && rubricFinite(n)
	case float64:
		return v, false, rubricFinite(v)
	default:
		// Numeric strings and legacy aliases are deliberately not coerced.
		return 0, false, false
	}
}

func rubricFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func normalizeRubricID(raw string) (string, bool) {
	var normalized strings.Builder
	separator := false
	for _, r := range strings.TrimSpace(raw) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			normalized.WriteRune(unicode.ToLower(r))
			separator = false
		} else if normalized.Len() > 0 && !separator {
			normalized.WriteByte('_')
			separator = true
		}
	}
	id := strings.Trim(normalized.String(), "_")
	return id, id != ""
}

func sortedRubricKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
