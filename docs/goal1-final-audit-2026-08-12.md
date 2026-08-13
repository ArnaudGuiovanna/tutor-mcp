# Audit final du Goal 1 — 2026-08-12

Cet audit clôt le gate « sécurité actuelle et conformité MCP/OAuth » défini
dans [`saas-goals.md`](./saas-goals.md). Il porte sur le profil mono-tenant
existant. Le modèle tenant, les clés composites tenant-aware et `FORCE RLS`
commencent au Goal 2 et ne sont donc ni simulés ni déclarés implicitement ici.

## Audit exigence par exigence

| Exigence de sortie | Preuve durable | Verdict |
|---|---|---|
| Une identité non vérifiée ne reçoit ni session ni token | Les parcours inscription/vérification/réutilisation et `TestUnverifiedLoginIsUniformCredentialFailure` gardent le compte inactif jusqu'à consommation atomique du token hashé. `TestAuthorizePost_RegisterRequiresEmailVerificationBeforeRedirect` interdit l'émission du code avant vérification. | `PASS` |
| Inscription, récupération et login ne permettent pas l'énumération | `TestRecoverResponseDoesNotEnumerateAccountsAndStoresOnlyTokenHash`, les réponses génériques d'inscription et `TestLoginAbsentAndLegacyExistingTimingDistributionsStayClose` couvrent contenu, stockage et distributions temporelles avec bcrypt réel. | `PASS` |
| La récupération est single-use et révoque les familles actives | `TestResetPasswordHTTPIsSingleUse` et les tests DB des tokens de compte/refresh valident consommation atomique, rotation et révocation de famille. | `PASS` |
| L'anti-bruteforce ne crée pas de verrouillage global attaquant-contrôlé | Le tracker applique pénalité progressive par compte/IP puis challenge de possession récupérable. Les tests couvrent cap mémoire, expiration, concurrence, fallback backend et annulation. Le compteur PostgreSQL atomique est borné et passe avec 124 writers concurrents. | `PASS` |
| RFC 8707 lie ressource, code, refresh et audience exacte | `TestHandleAuthorizeRequiresExactResource`, `TestAuthorizationCodeGrantRequiresMatchingResourceWithoutConsumingCode`, `TestRefreshGrantRequiresMatchingResourceWithoutRotating`, `TestVerifyJWTRejectsLegacyLogicalAudience` et les tests cross-client couvrent absence, mauvais resource, audience legacy et refresh inter-resource. | `PASS` |
| Redirect URI, PKCE, client et replay sont stricts | Les tests `UnregisteredRedirectURI`, `RedirectMismatchDoesNotConsume`, `PKCEMismatch`, `PublicClientStillRequiresPKCE`, confidential bad-secret, rotation/reuse concurrente de refresh et CSRF replay couvrent les frontières négatives. | `PASS` |
| CIMD est prioritaire et DCR fallback est borné/auditable | La résolution CIMD vérifie identité/URL/IP, cache LRU, single-flight et annulation. DCR possède modes `open|token|disabled`, IAT hashé, quota/TTL/révocation durables, déduplication concurrente, audit et commande opérateur. Douze inscriptions équivalentes concurrentes ne consomment qu'un client/quota. Aucun champ RFC 7592 fictif n'est annoncé. | `PASS` |
| MCP courant et legacy fonctionnent sans affinité | SDK Go v1.7.0, transport stateless, CORS courant et `TestStatelessMCPProtocolsWorkAcrossIndependentRoundRobinNodes` couvrent deux nœuds et pools indépendants, protocole `2026-07-28` et version legacy retenue. | `PASS` |
| Scopes et annotations correspondent aux effets des outils | La policy exhaustive des 45 outils est vérifiée par `TestRegisterTools_Smoke`, `TestAddToolEnforcesGranularOAuthScopes`, le refus write avec scope read avant claim idempotent, les challenges HTTP/MCP et les assertions de safety hints. | `PASS` |
| SQLite local et charge MCP restent bornés | Les tests `sqlite_permissions` imposent répertoire `0700`, fichiers DB/WAL/SHM `0600` et refusent parent/symlink dangereux. Limites de corps, quotas narratifs cumulés, concurrence globale/par principal et libération après deadline sont testés sous race. | `PASS` |
| Proxy, rate limits et dépendances ne sont pas usurpables ou infinies | La confiance XFF dépend du pair/CIDR approuvé et refuse un catch-all. Les appels backend portent une deadline et propagent l'annulation. `/live` est sans DB et `/ready` teste la dépendance. | `PASS` |
| Travail asynchrone et webhook reprennent après panne | Jobs et livraisons utilisent leases récupérables, heartbeats, tentatives bornées, DLQ, pagination keyset et pools bornés. Queue/réservation/historique partagent une machine d'état; `delivery_unknown` est mis en quarantaine sans retry aveugle. Les tests PostgreSQL couvrent crash, lease expiré, redelivery, rollback et reprise scheduler réelle. | `PASS` |
| Le runtime distribué ne dépend pas d'un disque narratif local | `NarrativeStore` partagé chiffre, versionne et vérifie checksum/AAD; CAS, journal de mutations, quotas et backfill sont durables. Deux stores voient le même état, les appends concurrents sont préservés et le test MCP ne crée aucun fichier local. | `PASS` |
| Les requêtes actives et fan-outs sont bornés | Le read-model contexte utilise cinq requêtes constantes. Les gates PostgreSQL 200/10k/100k respectent le p95 75 ms et exigent un plan indexé à 100k. Les traversées scheduler 10k/100k restent en pages de 128 et le pool isole les cibles lentes. | `PASS` |
| Le profil production échoue fermé sur TLS et secrets | `TestProductionProfileFailsClosed` refuse origine publique HTTP, proxy TLS incohérent, PostgreSQL sans `verify-full`/CA, mémoire locale distribuée, DCR ouvert et keyring absent. SMTP exige STARTTLS. | `PASS` |
| Un dump DB seul n'expose pas les credentials d'intégration | Les enveloppes AES-256-GCM versionnées sont liées par AAD et déchiffrées après claim, juste avant HTTP. Les tests couvrent ancienne/mauvaise clé, tag altéré, rotation atomique, rollback et absence de secret dans logs, erreurs et panics. | `PASS` |
| La rétention est reprenable, prouvable et respecte les legal holds | Chaque apply possède manifeste immuable, preuve de backup de moins de 24 h, bail et checkpoints. DB+preuve sont atomiques; narrative est idempotente. Les rapports distinguent `eligible/applied/held`; les tests SQLite/PostgreSQL/race couvrent crash partiel, reprise, concurrence, backup périmé et hold relationnel/local/partagé. Le runbook documente restauration et réconciliation. | `PASS` |

## Matrice exécutée

Toutes les commandes suivantes ont été exécutées sur l'arbre de travail final
du Goal 1 :

```bash
go test -count=1 -timeout=8m ./...
go test -race -count=1 -timeout=8m ./...
go vet ./...
staticcheck ./...
govulncheck ./...
git diff --check
```

Résultats : suite SQLite complète verte ; race SQLite vert (`auth` 426,5 s,
`db` 185,4 s, `engine` 477,3 s, `tools` 137,7 s) ; vet/staticcheck/diff verts ;
`govulncheck` ne trouve aucune vulnérabilité appelée (zéro dans le code et les
packages importés).

La commande exacte du job PostgreSQL est également verte :

```bash
TUTOR_TEST_PG_DSN=... TUTOR_LOAD_TEST=1 \
  go test -race -count=1 -timeout=12m ./...
```

Sur PostgreSQL 17 local : `db` 598,5 s, `engine` 488,2 s, `auth` 394,6 s,
`tools` 130,7 s. Elle active la charge de 50 apprenants/2 000 transactions,
les handlers OAuth, la boucle MCP de progression et la reprise scheduler sur
le dialecte réel. Les pools de fixtures sont bornés comme le runtime. Les tests
dédiés au migrateur continuent d'exécuter `MigratePostgres`; seuls les fixtures
métier matérialisent le schéma courant en une transaction avec les mêmes
checksums afin de tenir le budget CI.

## Décision

Le Goal 1 est `DONE`. Aucun échec ou waiver n'est ouvert. Les scénarios RLS A/B
restent explicitement au Goal 2, après introduction du contrat tenant et de la
RLS réelle ; ils ne peuvent pas être validés honnêtement sur le schéma
mono-tenant clôturé ici.
