# Audit final du Goal 4 — 2026-08-12

Cet audit clôt M5 et la définition de « SaaS MVP prêt ». M6 — partitionnement,
analytics hors OLTP, cellules et DDL online avancé — reste post-MVP sur mesure.

## Audit exigence par exigence

| Exigence | Preuve durable | Verdict |
|---|---|---|
| Control plane tenant exploitable | CLI PostgreSQL fail-closed pour provisionnement, statut, flags, domaines personnalisés, plans et abonnements. Motif/request ID obligatoires ; le secret de validation domaine est rendu une fois. Toutes les mutations sont transactionnelles et auditées. | `PASS` |
| Plans, entitlements et quotas résistent à la concurrence | Réservation atomique avant effet, consommation/libération/expiration idempotentes, périodes trial/active/grace et baisse de plan sûre. Le gate noisy-neighbour effectue 400 réservations concurrentes du gros tenant sans faire dépasser le petit. | `PASS` |
| Usage et billing sont réconciliables | `usage_events` et corrections append-only, clé serveur idempotente et rollup reproductible. Les webhooks billing sont HMAC-vérifiés, dédupliqués par provider/event et une panne passe en grâce plutôt qu'en coupure immédiate. | `PASS` |
| Audit privilégié est corrélé | `audit_events` tenant et `platform_audit_events` global sont append-only. Acteur, membership, action, cible, résultat, motif, request et trace sont stockés ; filtres tenant/action/cible/période sont paginés. Admin/support/billing/control plane et impersonation break-glass sont couverts. | `PASS` |
| OpenTelemetry et SLO sont actionnables | W3C HTTP, spans MCP/worker, tenant/membership pseudonymisés, résultats, durées, queue lag/transitions et pool SQL sans DSN/payload. [`saas-slo.md`](./saas-slo.md) fixe disponibilité, p95/p99, lag, RPO/RTO et seuils d'alerte. | `PASS` |
| Readiness et rollout échouent fermés | `/live` n'interroge pas la DB ; `/ready` vérifie ping + ledger. Production sépare les rôles, exige TLS/keyrings/shared state. Canary par flag/domaine et rollback N/N-1 additif sont documentés. | `PASS` |
| RGPD tenant est reprenable | Politiques de rétention versionnées, legal holds, checksums, DSAR export/rectification/effacement par phases et lots. Le worker consomme réellement le job d'export ; un hold bloque l'effacement et la même demande reprend après levée. | `PASS` |
| Noisy neighbour respecte le budget | Le gate PostgreSQL crée gros/petit tenants, lance 400 réservations concurrentes et mesure le petit tenant à p95 151,67 ms sur l'arbre final, sous le budget 2 s, avec quota exact. | `PASS` |
| Sauvegarde durable est chiffrée | `postgres-backup.sh` diffuse le custom dump directement dans CMS AES-256-GCM, crée manifestes/hash/permissions 0600 sans overwrite et garde la clé privée séparée. Le déchiffrement du test 2026-08-12 a produit un catalogue `pg_restore` valide. | `PASS` |
| PITR est réellement exercé | `pitr-restore-exercise.sh` fait base backup, archivage WAL, cible LSN, replay et promotion. PostgreSQL 17 : WAL 553 ms, recovery 1 908 ms ; marqueurs `base-backup`/`before-target` présents, `after-target` absent. | `PASS` |
| Restauration complète et logique tenant sont vérifiées | Full restore chiffré compare les checksums. L'archive logique capture exactement un tenant et ses dépendances globales minimales, vérifie SHA-256/inventaire/FK/RLS/isolation, refuse altération/collision et restaure une narrative déchiffrable dans un schéma indépendant. Rôle restore dédié testé réellement. | `PASS` |

## Gates de release

Cette section est mise à jour depuis l'arbre final après tout correctif de gate.

| Gate | Résultat final |
|---|---|
| Suite Go SQLite complète | `PASS` — `go test -timeout=15m ./... -count=1`; notamment db 21,73 s, engine 76,02 s et tools 17,10 s. |
| PostgreSQL + charge/isolation/restauration | `PASS` — suite db exhaustive 1 483,58 s ; noisy-neighbour 400 réservations, petit tenant p95 151,67 ms ; RLS, archive logique et gouvernance ciblées vertes. |
| `go test -race ./...` | `PASS` — exécution consolidée finale avec `-p 1 -timeout=45m`; auth 373,07 s, db 330,81 s, engine 1 333,14 s et tools 208,94 s. Le stress de 50 writers passe aussi 10 fois normalement et 3 fois sous race. |
| `go vet`, `staticcheck`, `govulncheck` | `PASS` — aucune alerte ; gRPC 1.82.1 corrige GO-2026-6061 et aucun symbole vulnérable n'est appelé. |
| Scripts shell, sauvegarde chiffrée, PITR, diff | `PASS` — `bash -n deploy/*.sh`, `git diff --check`, backup CMS-GCM 0600 relisible, full restore isolé `verified:true`, PITR réel LSN/promotion. |

## Décision

Goal 4 : `DONE` au 2026-08-13. Tous les gates ont été exécutés sur l'arbre final,
sans waiver. Les deux bases et les clés de l'exercice de restauration ont été
supprimées après vérification ; M6 reste volontairement post-MVP et piloté par
les mesures de production.
