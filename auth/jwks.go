// Copyright (c) 2026 Arnaud Guiovanna <https://www.aguiovanna.fr>
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
)

type jwk struct {
	KTY string `json:"kty"`
	CRV string `json:"crv"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	KID string `json:"kid"`
	X   string `json:"x"`
}

// HandleJWKS publishes verification material only. Private bytes never enter
// the response or logs. HS256 compatibility mode intentionally publishes an
// empty set because symmetric secrets cannot be exposed as JWKS.
func HandleJWKS(w http.ResponseWriter, _ *http.Request) {
	signingKeys.RLock()
	keys := make([]jwk, 0, len(signingKeys.public))
	for kid, publicKey := range signingKeys.public {
		keys = append(keys, jwk{
			KTY: "OKP", CRV: "Ed25519", Use: "sig", Alg: "EdDSA", KID: kid,
			X: base64.RawURLEncoding.EncodeToString(publicKey),
		})
	}
	signingKeys.RUnlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i].KID < keys[j].KID })
	w.Header().Set("Content-Type", "application/jwk-set+json")
	w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
	_ = json.NewEncoder(w).Encode(struct {
		Keys []jwk `json:"keys"`
	}{Keys: keys})
}
