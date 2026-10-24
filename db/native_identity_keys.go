// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// NormalizeNativePublicKey accepts NIP-19 npub or BIP-340 x-only hexadecimal
// public keys. It deliberately never accepts or decodes an nsec private key.
func NormalizeNativePublicKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if validNativePublicKey(strings.ToLower(value)) {
		return strings.ToLower(value), nil
	}
	if len(value) != 63 || (value != strings.ToLower(value) && value != strings.ToUpper(value)) {
		return "", fmt.Errorf("expected an npub or 64-character secp256k1 public key")
	}
	value = strings.ToLower(value)
	if !strings.HasPrefix(value, "npub1") {
		return "", fmt.Errorf("expected a public npub key; private keys are never accepted")
	}
	const alphabet = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	values := make([]byte, 0, 58)
	for _, letter := range value[5:] {
		index := strings.IndexRune(alphabet, letter)
		if index < 0 {
			return "", fmt.Errorf("invalid npub encoding")
		}
		values = append(values, byte(index))
	}
	checksum := uint32(1)
	update := func(v byte) {
		top := checksum >> 25
		checksum = (checksum&0x1ffffff)<<5 ^ uint32(v)
		for i, g := range []uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3} {
			if top>>i&1 != 0 {
				checksum ^= g
			}
		}
	}
	for _, c := range "npub" {
		update(byte(c) >> 5)
	}
	update(0)
	for _, c := range "npub" {
		update(byte(c) & 31)
	}
	for _, v := range values {
		update(v)
	}
	if checksum != 1 {
		return "", fmt.Errorf("invalid npub checksum")
	}
	var accumulator uint32
	bits := 0
	raw := make([]byte, 0, 32)
	for _, v := range values[:len(values)-6] {
		accumulator = (accumulator << 5) | uint32(v)
		bits += 5
		for bits >= 8 {
			bits -= 8
			raw = append(raw, byte(accumulator>>bits))
		}
	}
	if bits >= 5 || (accumulator<<(8-bits))&255 != 0 || len(raw) != 32 {
		return "", fmt.Errorf("invalid npub padding")
	}
	normalized := hex.EncodeToString(raw)
	if !validNativePublicKey(normalized) {
		return "", fmt.Errorf("invalid secp256k1 public key")
	}
	return normalized, nil
}
