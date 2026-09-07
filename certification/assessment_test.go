package certification

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func signedEnvelope(t *testing.T, private ed25519.PrivateKey, payload string) string {
	t.Helper()
	e := Envelope{KeyID: "key", Payload: base64.RawURLEncoding.EncodeToString([]byte(payload))}
	e.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(MessagePrefix+e.KeyID+"\n"+e.Payload)))
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAssessmentCertificationBindsAuthorityAndFreshClaims(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	configuration, _ := json.Marshal([]Authority{{KeyID: "key", AuthorityID: "independent", TenantID: "tenant", PublicKey: base64.RawURLEncoding.EncodeToString(public)}})
	v, err := New("audience", string(configuration))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	claims := Claims{ID: "certificate", Audience: "audience", TenantID: "tenant", AttemptID: "attempt", ReviewID: "review", MaterialHash: strings.Repeat("a", 64), ScoreHash: strings.Repeat("b", 64), Verdict: "accept", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}
	raw, _ := json.Marshal(claims)
	valid := signedEnvelope(t, private, string(raw))
	verified, err := v.Verify(valid, now)
	if err != nil || verified.Claims() != claims || verified.Authority().AuthorityID != "independent" || len(verified.Hash()) != 64 {
		t.Fatalf("verify: %+v %v", verified, err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Claims)
	}{
		{"audience", func(c *Claims) { c.Audience = "other" }},
		{"tenant", func(c *Claims) { c.TenantID = "other" }},
		{"expired", func(c *Claims) { c.ExpiresAt = now.Unix() }},
		{"future", func(c *Claims) { c.IssuedAt++ }},
		{"long lived", func(c *Claims) { c.ExpiresAt += 1000 }},
		{"unknown verdict", func(c *Claims) { c.Verdict = "human_review" }},
		{"revision", func(c *Claims) { c.ExpectedRevision = -1 }},
		{"hash", func(c *Claims) { c.ScoreHash = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := claims
			tc.change(&c)
			raw, _ := json.Marshal(c)
			if _, err := v.Verify(signedEnvelope(t, private, string(raw)), now); err == nil {
				t.Fatal("accepted invalid claim")
			}
		})
	}
	for _, payload := range []string{
		strings.TrimSuffix(string(raw), "}") + `,"verdict":"reject"}`,
		strings.TrimSuffix(string(raw), "}") + `,"human_review":true}`,
		string(raw) + `{}`,
	} {
		if _, err := v.Verify(signedEnvelope(t, private, payload), now); err == nil {
			t.Fatal("accepted ambiguous or extended payload")
		}
	}
	_, wrongKey, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := v.Verify(signedEnvelope(t, wrongKey, string(raw)), now); err == nil {
		t.Fatal("accepted another signer")
	}
	disabled, _ := New("audience", "")
	if _, err := disabled.Verify(valid, now); err == nil {
		t.Fatal("disabled verifier minted trust")
	}
	for _, config := range []string{`null`, `[]`, `[{}]`, strings.TrimSuffix(string(configuration), "]") + "," + string(configuration[1:])} {
		if _, err := New("audience", config); err == nil {
			t.Fatal("accepted invalid authority configuration")
		}
	}
}
