// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"strings"
	"testing"
	"time"
)

func TestNativeActionProofBindsRequestAndRejectsAmbiguity(t *testing.T) {
	now := time.Now().UTC()
	url := "https://institution.example/api/client/v1/institution/actions"
	tenant, session := "institution-a", "native-session-a"
	body := []byte(`{"action":"overview","input":{}}`)
	payload := sha256.Sum256(body)
	key := nativeTestKey(91)
	original := nativeNostrEvent{PublicKey: nativeTestPub(key), CreatedAt: now.Unix(), Kind: nativeEventKind, Tags: [][]string{{"u", url}, {"method", "POST"}, {"payload", hex.EncodeToString(payload[:])}, {"nonce", "unique-host-nonce-001"}, {"tenant", tenant}, {"session", session}}, Content: ""}
	sign := func(event nativeNostrEvent) string {
		t.Helper()
		id := nativeEventID(event)
		event.ID = hex.EncodeToString(id[:])
		sig, err := schnorr.Sign(key, id[:])
		if err != nil {
			t.Fatal(err)
		}
		event.Signature = hex.EncodeToString(sig.Serialize())
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(raw)
	}
	header := sign(original)
	proof, err := VerifyNativeActionProof(header, url, tenant, session, body, now)
	if err != nil || proof.PublicKey != original.PublicKey || proof.EventID == "" || proof.TenantID != tenant || proof.SessionID != session {
		t.Fatalf("valid proof=%+v %v", proof, err)
	}
	if _, err = VerifyNativeActionProof("Nostr "+header, url, tenant, session, body, now); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyNativeActionProof(header, url+"/other", tenant, session, body, now); err == nil {
		t.Fatal("proof accepted for another URL")
	}
	if _, err = VerifyNativeActionProof(header, url, tenant, session, append(body, ' '), now); err == nil {
		t.Fatal("proof accepted for changed request bytes")
	}
	if _, err = VerifyNativeActionProof(header, url, "institution-b", session, body, now); err == nil {
		t.Fatal("proof replay accepted in another institution for the same signing key")
	}
	if _, err = VerifyNativeActionProof(header, url, tenant, "native-session-b", body, now); err == nil {
		t.Fatal("proof replay accepted in another native session for the same signing key")
	}
	for _, change := range []string{"stale", "future", "duplicate", "method", "content", "nonce", "extra"} {
		t.Run(change, func(t *testing.T) {
			event := original
			event.Tags = append([][]string(nil), original.Tags...)
			switch change {
			case "stale":
				event.CreatedAt = now.Add(-61 * time.Second).Unix()
			case "future":
				event.CreatedAt = now.Add(61 * time.Second).Unix()
			case "duplicate":
				event.Tags[3] = []string{"u", url}
			case "method":
				event.Tags[1] = []string{"method", "GET"}
			case "content":
				event.Content = "unexpected"
			case "nonce":
				event.Tags[3] = []string{"nonce", "short"}
			case "extra":
				event.Tags = append(event.Tags, []string{"other", "unbound"})
			}
			if _, err := VerifyNativeActionProof(sign(event), url, tenant, session, body, now); err == nil {
				t.Fatal("invalid proof accepted")
			}
		})
	}
	if _, err = VerifyNativeActionProof(strings.Repeat("a", 20*1024), url, tenant, session, body, now); err == nil {
		t.Fatal("unbounded header accepted")
	}
}
