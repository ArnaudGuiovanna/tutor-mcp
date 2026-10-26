// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

const nativeEventKind = 27235

type nativeNostrEvent struct {
	ID        string     `json:"id"`
	PublicKey string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Signature string     `json:"sig"`
}

func nativeHex(value string, size int) ([]byte, error) {
	if len(value) != size*2 || strings.ToLower(value) != value {
		return nil, errors.New("non-canonical hex")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil, errors.New("invalid hex")
	}
	return decoded, nil
}

func nativePublicKey(value string) bool {
	raw, err := nativeHex(value, 32)
	if err != nil {
		return false
	}
	_, err = schnorr.ParsePubKey(raw)
	return err == nil
}

// NIP-01 requires UTF-8 JSON without HTML/U+2028/U+2029 escaping. Go's JSON
// encoder always escapes the last two, even with SetEscapeHTML(false), so the
// canonical event array uses this small string encoder instead.
func appendNativeJSONString(dst []byte, value string) []byte {
	dst = append(dst, '"')
	for _, char := range value {
		switch char {
		case '"', '\\':
			dst = append(dst, '\\', byte(char))
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		default:
			if char < 0x20 {
				const hexDigits = "0123456789abcdef"
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[char>>4], hexDigits[char&15])
			} else {
				dst = utf8.AppendRune(dst, char)
			}
		}
	}
	return append(dst, '"')
}

func nativeEventID(event nativeNostrEvent) [32]byte {
	canonical := appendNativeJSONString([]byte("[0,"), event.PublicKey)
	canonical = append(canonical, ',')
	canonical = strconv.AppendInt(canonical, event.CreatedAt, 10)
	canonical = append(canonical, ',')
	canonical = strconv.AppendInt(canonical, int64(event.Kind), 10)
	canonical = append(canonical, ',', '[')
	for i, tag := range event.Tags {
		if i != 0 {
			canonical = append(canonical, ',')
		}
		canonical = append(canonical, '[')
		for j, value := range tag {
			if j != 0 {
				canonical = append(canonical, ',')
			}
			canonical = appendNativeJSONString(canonical, value)
		}
		canonical = append(canonical, ']')
	}
	canonical = append(canonical, ']', ',')
	canonical = appendNativeJSONString(canonical, event.Content)
	canonical = append(canonical, ']')
	return sha256.Sum256(canonical)
}

func verifyNativeNostr(header string, prefixRequired bool, publicKey string, expected map[string]string, now time.Time) error {
	return verifyNativeNostrWindow(header, prefixRequired, publicKey, expected, now, 120, 30)
}

func verifyNativeNostrWindow(header string, prefixRequired bool, publicKey string, expected map[string]string, now time.Time, pastSeconds, futureSeconds int64) error {
	if len(header) > 16*1024 || strings.ContainsAny(header, "\r\n\t") {
		return errors.New("invalid Nostr authorization")
	}
	encoded := header
	if scheme, token, ok := strings.Cut(header, " "); ok && strings.EqualFold(scheme, "Nostr") {
		encoded = token
	} else if prefixRequired {
		return errors.New("missing Nostr authorization")
	}
	if strings.ContainsAny(encoded, " \r\n\t") {
		return errors.New("invalid Nostr authorization")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > 12*1024 || !utf8.Valid(raw) {
		return errors.New("invalid Nostr authorization")
	}
	var event nativeNostrEvent
	if err := decodeNativeJSON(raw, &event); err != nil {
		return errors.New("invalid Nostr event")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || len(members) != 7 {
		return errors.New("incomplete Nostr event")
	}
	for _, field := range []string{"id", "pubkey", "created_at", "kind", "tags", "content", "sig"} {
		value, exists := members[field]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("incomplete Nostr event")
		}
	}
	if event.Kind != nativeEventKind || event.Content != "" || event.PublicKey != publicKey ||
		event.CreatedAt < now.Unix()-pastSeconds || event.CreatedAt > now.Unix()+futureSeconds || len(event.Tags) != len(expected) {
		return errors.New("invalid Nostr event binding")
	}
	seen := make(map[string]bool, len(expected))
	for _, tag := range event.Tags {
		if len(tag) != 2 || seen[tag[0]] {
			return errors.New("ambiguous Nostr tag")
		}
		value, exists := expected[tag[0]]
		if !exists || value != tag[1] {
			return errors.New("invalid Nostr tag binding")
		}
		seen[tag[0]] = true
	}
	pubRaw, err := nativeHex(event.PublicKey, 32)
	if err != nil {
		return err
	}
	pub, err := schnorr.ParsePubKey(pubRaw)
	if err != nil {
		return errors.New("invalid Nostr public key")
	}
	sigRaw, err := nativeHex(event.Signature, 64)
	if err != nil {
		return err
	}
	signature, err := schnorr.ParseSignature(sigRaw)
	if err != nil {
		return errors.New("invalid Nostr signature")
	}
	id := nativeEventID(event)
	if event.ID != hex.EncodeToString(id[:]) || !signature.Verify(id[:], pub) {
		return errors.New("invalid Nostr signature")
	}
	return nil
}

// Reject duplicate keys before decoding structs: encoding/json otherwise
// accepts the last duplicate, which makes signed wire contracts ambiguous.
func decodeNativeJSON(raw []byte, destination any) error {
	if !utf8.Valid(raw) {
		return errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 16 {
			return errors.New("JSON is too deep")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				if err != nil || !ok || seen[key] {
					return errors.New("duplicate or invalid JSON key")
				}
				seen[key] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case json.Delim('['):
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}
