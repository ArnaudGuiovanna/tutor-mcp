# MVP — exit criteria checklist

Source-of-truth tracker for the five MVP categories defined in the agent routine. Each row is updated when an open PR or merged commit changes its evidence link.

> **Status convention.** ✅ = met today. 🟡 = partial / further hardening
> planned. ❌ = not addressed. N/A = deliberately outside the server
> architecture.
>
> **Deployment target.** The MVP gate covers the SQLite single-node profile.
> PostgreSQL is exercised for conformance, while multi-node operation remains
> experimental until shared narrative memory and crash-safe jobs/outbox
> delivery exist.

## 1. Fonctionnel

| Item | Status | Evidence |
|------|--------|----------|
| Tous les tools renvoient une réponse valide sur cas nominaux | ✅ | `go test ./tools/` green; happy-path and domain-isolation E2E coverage in `tools/interaction_e2e_test.go` |
| Un client MCP générique peut exécuter la boucle tutorale authentifiée | ✅ | `main_test.go::TestStreamableHTTPClientCompletesTutoringLoop` uses the official client over the real Streamable HTTP handler, fetches `tutor_mcp`, discovers tools, then persists one evidence update |
| Entrées vides / malformées gérées proprement | ✅ | `tools/length_validation_test.go`, `tools/validate_test.go`, enum/unit-interval tests, and MCP jsonschema validation |
| Schémas I/O cohérents avec descriptions | ✅ | Canonical output-shape tests cover `mastery`, `global_progress_percent`, and `concept` without legacy output aliases (#86); `concept_id` remains input-only compatibility |

## 2. Pédagogique

| Item | Status | Evidence |
|------|--------|----------|
| BKT / FSRS / IRT update after `record_interaction` | ✅ | `algorithms/*_test.go`, `tools/interaction_test.go` |
| `get_next_activity` cohérent avec l'état cognitif | ✅ | `tools/activity_test.go`, domain-scoped state/history tests, and shared-label routing E2E in `tools/interaction_e2e_test.go` |
| E2E « créer un domaine → 5 exercices → mastery progresse » | ✅ | `tools/interaction_e2e_test.go::TestEndToEnd_TenSuccessesMoveMastery` |
| E2E coherence multi-tool (archived/deleted domain, multi-domain, calibration round-trip) | ✅ | `tools/coherence_e2e_test.go`, `tools/manage_domain_test.go`, `tools/negotiation_test.go`, and `tools/interaction_e2e_test.go` |

## 3. Signal LLM

| Item | Status | Evidence |
|------|--------|----------|
| Sorties JSON structurées (mastery, confidence, hints) | ✅ | `jsonResult` + `StructuredContent` in `tools/tools.go` |
| Descriptions de tools sans ambiguïté pour le routing | 🟡 | PR #104 covers the alerts/activity/mirror trio (#84); other tools still rely on legacy descriptions |
| Champs documentés et utilisés downstream | ✅ | `get_learner_context` exposes `priority_concept_domain_id` for routable priority concepts (#154); dashboard units document 0..1 vs 0..100 fields |

## 4. Sécurité

| Item | Status | Evidence |
|------|--------|----------|
| Validation des inputs sur tous les endpoints | ✅ | Known enums, finite/range validation, text/collection caps and NaN/Inf transport guards are covered in `tools/*_test.go` and `auth/*_test.go` |
| Rate-limiting sur endpoints coûteux | ✅ | `auth.RateLimitMiddleware` on /authorize, /token, /register, /mcp (`main.go:113-121`) |
| Injection impossible via champs texte | ✅ | All DB writes use parameterised SQL; jsonschema enforces basic typing |
| Defence-in-depth: per-learner DB filters | ✅ | Calibration completion/read filters include `learner_id`; domain-scoped calibration/transfer/state/history regressions live in `db/domain_scoping_test.go` |
| Pas de secrets en clair dans les logs | ✅ | `auth/jwt.go` redacts secrets; `requestLogger` logs only method/path/status/UA |
| Domain ownership / archived enforcement | ✅ | `resolveDomain`, domain-scoped queries, `tools/manage_domain_test.go`, and shared-label E2E coverage |

## 5. UX

| Item | Status | Evidence |
|------|--------|----------|
| Messages d'erreur explicites et actionnables | 🟡 | `errorResult(...)` everywhere; mixed-language gap tracked by issue #90 (not addressed) |
| Latence p95 < 2 s (tools sans LLM) | ✅ | Opt-in CI/release gate in `tools/activity_pair_perf_test.go` covers `get_pending_alerts` + `get_next_activity`; observed p95 17.98 ms at 200 active learners with `MCP_PERF_BUDGET=1 MCP_PERF_ACTIVE_LEARNERS=200` |
| Latence p95 < 8 s (tools avec sampling) | N/A | Tutor MCP does not sample an LLM server-side; host-model latency belongs to the MCP client |
| README à jour avec quickstart vérifié | ✅ | Documentation matches the supported single-node profile; the generic-client quickstart path is exercised by `TestStreamableHTTPClientCompletesTutoringLoop` |

## Decision gates

- The automated code gate for the supported SQLite single-node profile is
  green. A release still requires a documented staging smoke test with at
  least one external MCP host, because the server cannot force a host LLM to
  load the prompt or close the `get_next_activity` → `record_interaction`
  loop.
- Mixed-language errors and the remaining legacy tool descriptions are
  hardening work, not data-integrity blockers.
- PostgreSQL multi-node is outside this MVP gate until shared narrative memory,
  crash-safe jobs/outbox delivery, and load/failure tests exist.
- Version bumps, tags, and the `staging` → `main` promotion remain human-only
  release steps.
