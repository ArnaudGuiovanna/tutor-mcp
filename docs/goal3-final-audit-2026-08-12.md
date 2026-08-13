# Audit final du Goal 3 — 2026-08-12

Cet audit clôt M4, le runtime horizontal et les données partagées.

## Audit exigence par exigence

| Exigence | Preuve durable | Verdict |
|---|---|---|
| API, worker et migrateur sont séparables | `PROCESS_ROLE=api|worker|migrator`; le profil production refuse `all`. Seul le migrateur acquiert le lock et applique la DDL ; API/worker vérifient le ledger en lecture seule et leurs privilèges. | `PASS` |
| MCP reste stateless sans affinité | Le principal est revérifié à chaque requête ; aucune donnée métier n'est attachée au transport. `TestStatelessMCPProtocolsWorkAcrossIndependentRoundRobinNodes` alterne deux nœuds/pools sur le protocole courant et legacy. | `PASS` |
| Mémoire narrative partagée et canonique | PostgreSQL `narrative_objects` fournit AES-256-GCM, AAD tenant/enrollment, checksum, CAS/version, mutation idempotente, quotas et rotation. Deux stores voient les mêmes objets ; écritures concurrentes et clé identique dans deux enrollments restent isolées. | `PASS` |
| Rate limits et browser state sont fleet-wide | Le profil distribué exige le backend PostgreSQL : buckets login/MCP, CSRF/nonce et routing de credentials sont durables et tenant-aware. Le démarrage refuse une combinaison process-local. | `PASS` |
| Mutation et événement sont atomiques | Publication de formation + outbox partagent une transaction. `TestFormationPublishAndOutboxAreAtomic` prouve commit et rollback sans événement fantôme. | `PASS` |
| Jobs/outbox reprennent après crash | Claims avec lease/heartbeat, tentative bornée, backoff, idempotence et DLQ. Les tests couvrent lease expiré, crash/reclaim, poison job, rollback, heartbeat et relance scheduler PostgreSQL réelle. | `PASS` |
| Un worker lent ne monopolise pas la flotte | Tenants paginés par 100, work pools bornés, transactions tenant courtes ; les appels externes se font hors transaction avec timeout. Les tests de pool prouvent deadline/stop gracieux et absence de blocage des voisins. | `PASS` |
| Webhooks SaaS sont tenant-scoped et vérifiables | Endpoint HTTPS allowlisté, secret chiffré/versionné, HMAC timestamp/event/body, `event_id` stable, historique durable, huit tentatives et DLQ. Rotation avec overlap 5 min–7 j et tests de signature/isolation/redelivery. | `PASS` |
| Perte d'un nœud n'entraîne ni perte ni double effet durable | Le relay/job est idempotent et une livraison déjà enregistrée 2xx ferme la fenêtre crash avant complétion du job. Les leases scheduler sont fenced. Direct Discord garde honnêtement la quarantaine `delivery_unknown`. | `PASS` |
| Le travail horizontal est observable et audité | `worker_tenant_runs`, spans OTel par job/tenant pseudonymisé, compteurs/durées, lag et transitions de queue. Les erreurs sont des codes bornés sans payload, URL ou secret. | `PASS` |

## Rollout et rollback

Le runbook [`saas-runtime-operations.md`](./saas-runtime-operations.md) impose
migration unique, worker/API canary, readiness, drain puis expansion. N accepte
les entrées additives N+1 du ledger. Un rollback arrête N+1, draine ses jobs,
désactive son flag et relance N sans retirer le schéma.

## Décision

Goal 3 : `DONE`. Aucun broker séparé n'est requis au MVP : PostgreSQL constitue
l'outbox/queue partagée avec `SKIP LOCKED`, leases et pagination. Cette décision
réduit les composants sans affaiblir le contrat durable testé.
