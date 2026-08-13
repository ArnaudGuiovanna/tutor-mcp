# Inventaire de propriété des données tenant

État de référence : 2026-08-12, schémas SQLite `0063` et PostgreSQL
`postgres_0054`. Cette matrice est normative pour la migration expand/contract :
une ligne non classée ne doit pas recevoir de nouvelle écriture.

## Règles de classement et d'anomalie

- **Global** : identité ou contrôle commun à la plateforme, sans donnée
  pédagogique. Son accès passe par un port de control plane explicite.
- **Tenant-owned** : la ligne appartient à exactement un `tenant_id`; une clé,
  une unicité ou une FK métier inclut ce tenant.
- **Dérivée** : état reconstruisible, cache, compteur ou journal technique. Le
  scope de la source est néanmoins conservé pour l'isolation et l'effacement.
- Le tenant legacy stable est `tenant_legacy`. Un rattachement est automatique
  uniquement lorsque la FK `learner_id` existante prouve ce propriétaire.
- Une ligne orpheline, une relation `learner_id`/`domain_id` contradictoire ou
  un concept dont le domaine n'est pas unique va dans
  `tenant_migration_quarantine`; elle n'est jamais rattachée par email, libellé,
  minimum d'ID ou autre heuristique silencieuse.
- Une quarantaine conserve table, clé primaire, motif, empreinte du contenu,
  dates de détection/résolution et décision opérateur. Elle exclut les secrets
  et le contenu pédagogique en clair.

## Tables actuelles

Volumétrie : `XS` configuration bornée, `S` par tenant, `M` croissance par
activité, `H` flux potentiellement non borné.

| Table | Classe / propriétaire cible | Volume | Rétention cible | Backfill et contrôle d'anomalie |
|---|---|---:|---|---|
| `schema_migrations` | Global / migrateur | XS | durée de vie DB | Aucun tenant; checksum strict. |
| `tenants` *(cible)* | Global / control plane | S | contrat + obligations légales | Créer `tenant_legacy` de façon idempotente. |
| `users` *(cible)* | Global / identité | S | vie du compte + purge RGPD | Un user par learner historique; aucune fusion sur email. |
| `external_identities` *(cible)* | Global / identité | S | vie du lien + audit | Aucun lien historique inventé; doublons issuer/subject en quarantaine. |
| `tenant_memberships` *(cible)* | Tenant-owned / tenant + user | S | vie membership + audit | Un membership legacy par learner; état/version explicites. |
| `tenant_identity_providers` | Tenant-owned / configuration IAM | XS | vie du contrat + audit | Aucun fournisseur inventé; issuer/kind exacts et état versionné. |
| `federated_identity_links` | Tenant-owned / provider + subject | S | vie du lien + audit | Le subject est l'identité; l'email n'est jamais une clé de fusion. |
| `service_accounts` | Tenant-owned / tenant | S | expiration/révocation + audit | Secret opaque haché, rôles délégués bornés et version de révocation. |
| `service_account_routes` | Global / route de credential | XS | TTL du credential | Hash uniquement; résout le tenant sans sélecteur client. |
| `support_access_grants` | Tenant-owned / accès break-glass | XS | TTL court + audit légal | Raison/request ID obligatoires, lecture seule, durée ≤ 1 h. |
| `support_access_routes` | Global / route de capability support | XS | TTL du grant | Hash uniquement; supprimé à la révocation. |
| `learners` | Tenant-owned / membership legacy | S | politique tenant/RGPD | `learner_id` prouve tenant/user/membership; profils restent séparés. |
| `oauth_clients` | Global ou installation explicitement tenant-scoped | XS | expiration + fenêtre d'audit | Les clients partagés restent globaux; aucune autorité tenant implicite. |
| `oauth_dcr_initial_access_tokens` | Global / control plane OAuth | XS | expiration/révocation + audit | Pas de backfill tenant; jeton haché, délégation future explicite. |
| `oauth_dcr_audit` | Global / audit OAuth | M | politique d'audit immuable | Client/token inconnus signalés, jamais réaffectés. |
| `oauth_codes` | Tenant-owned / membership | M | TTL court puis suppression | Tenant dérivé uniquement du learner FK; code orphelin purgé/quarantainé. |
| `refresh_tokens` | Tenant-owned / membership + client | M | TTL/révocation famille | Même règle que code; famille mixte tenant/client révoquée. |
| `learner_approved_clients` | Tenant-owned / membership + client | S | jusqu'au retrait du consentement | Backfill par learner; client absent interdit. |
| `account_tokens` | Tenant-owned / membership | M | TTL court + tombstone consommé | Backfill par learner; continuation OAuth contradictoire invalidée. |
| `login_challenges` | Tenant-owned / membership | M | TTL court + consommation | Backfill par learner; challenge orphelin invalidé. |
| `domains` | Tenant-owned / learner puis enrollment | S | archivage + RGPD | Backfill par learner; `domain_id` devient unique avec tenant. |
| `formations`, `formation_versions` | Tenant-owned / catalogue | S | versions publiées immuables | Un domain legacy devient une formation distincte; aucune déduplication par nom. |
| `formation_modules`, `formation_concepts`, `concept_prerequisites` | Tenant-owned / formation version | M | vie de la preuve référencée | FKs composites tenant/version; contenu publié immuable. |
| `cohorts`, `cohort_trainers` | Tenant-owned / formation version | S | archivage + audit | Version publiée figée; capacité et formateurs atomiques. |
| `enrollments` | Tenant-owned / cohort + membership + version | M | preuves/RGPD | Unicité cohorte/user; version figée et réinscription explicite. |
| `legacy_domain_enrollments`, `legacy_concept_sources`, `legacy_concept_mappings` | Tenant-owned / pont de migration | M | jusqu'au contract + audit | Mapping prouvé; ambiguïtés vers enrollment/concept de quarantaine explicites. |
| `curriculum_versions` | Tenant-owned / formation version | M | immuable tant que preuve référencée | Vérifier `(learner_id,domain_id)`; divergence en quarantaine. |
| `curriculum_concepts` | Tenant-owned / formation version | M | immuable tant que référencée | Domaine/learner doivent partager le tenant. |
| `curriculum_metadata_ids` | Tenant-owned / concept | M | immuable tant que référencée | Concept, domaine et learner doivent converger. |
| `concept_states` | Tenant-owned / enrollment + concept | H | politique progression/RGPD | Backfill learner→enrollment; domaine vide/ambigu en quarantaine. |
| `learner_concept_states` | Tenant-owned / enrollment + formation concept | H | politique progression/RGPD | Source canonique M3; PK tenant/enrollment/concept et double FK de version. |
| `learning_sessions` | Tenant-owned / enrollment | H | politique preuves/RGPD | Learner et domaine doivent converger; domaine NULL reste traçable. |
| `assessment_attempts` | Tenant-owned / enrollment + concept | H | politique preuves/formation | Learner, session, domaine et concept contrôlés ensemble. |
| `interactions` | Tenant-owned / enrollment + attempt | H | rétention preuves/RGPD | Contrôler learner/session/attempt/domain; aucune inférence de domaine ambigu. |
| `pedagogical_snapshots` | Tenant-owned / interaction | H | alignée sur interaction | Tenant vient de l'interaction; mismatch learner/domain quarantainé. |
| `affect_states` | Tenant-owned / enrollment + session | H | rétention courte configurable | Learner/session doivent converger. |
| `calibration_records` | Tenant-owned / enrollment + concept | H | politique progression | Domaine/concept ambigu reste quarantainé. |
| `transfer_records` | Tenant-owned / enrollment + attempt | H | politique progression | Contrôler learner/domain/attempt. |
| `implementation_intentions` | Tenant-owned / enrollment + session | M | politique progression/RGPD | Contrôler learner/domain/session. |
| `availability` | Tenant-owned / membership | S | vie du profil/RGPD | Backfill direct par learner. |
| `scheduled_alerts` | Tenant-owned / membership/enrollment | M | TTL livraison + audit minimal | Backfill par learner; réservation orpheline annulée. |
| `webhook_message_queue` | Tenant-owned / intégration tenant | H | TTL queue/DLQ bornée | Contrôler learner/domain/réservation; événement dupliqué quarantainé. |
| `webhook_delivery_transitions` | Tenant-owned / événement queue | H | audit livraison borné | Tenant vient de `queue_id`; learner discordant quarantainé. |
| `webhook_push_log` | Tenant-owned / intégration tenant | H | politique notification/RGPD | Backfill par learner; domaine discordant quarantainé. |
| `pending_consolidations` | Tenant-owned / enrollment | M | jusqu'à consolidation + fenêtre audit | Backfill par learner. |
| `narrative_objects` | Tenant-owned / enrollment | H | politique mémoire/RGPD | Backfill par learner; clé objet canonique et checksum vérifiés. |
| `narrative_mutations` | Tenant-owned / objet narratif | H | audit mutation borné | Tenant vient de l'objet; objet manquant quarantainé. |
| `tool_call_idempotency` | Tenant-owned / membership/enrollment | H | réponse expirée, tombstone conservé | Backfill par learner; clé future `(tenant,actor,tool,key)`. |
| `retention_legal_holds` | Tenant-owned / sujet learner | S | durée du hold + audit | Backfill par learner; hold orphelin bloque la purge et alerte. |
| `retention_jobs` | Tenant-owned / sujet learner | M | audit de purge sans contenu | Backfill par learner; job sans sujet valide mis en échec. |
| `retention_job_phases` | Tenant-owned / job de rétention | M | alignée sur job | Tenant vient du job; phase orpheline quarantainée. |
| `rate_limit_buckets` | Dérivée / tenant + acteur + IP + action | M | fenêtre + marge opérationnelle | Anciennes clés non décodables expirent; pas de tenant deviné. |
| `login_failures` | Dérivée / identité globale puis tenant si sélectionné | H | fenêtre de défense courte | Journal legacy vidé après agrégation; aucune donnée d'autorisation. |
| `login_failure_windows` | Dérivée / identité globale puis tenant si sélectionné | M | fenêtre + marge courte | Clés existantes expirent; nouveau format versionné/scopé. |
| `scheduled_job_runs` | Dérivée / worker global ou tenant explicite | M | historique opérationnel borné | Les jobs globaux restent marqués globaux; aucune exécution tenant implicite. |
| `tenant_migration_quarantine` *(cible)* | Global / migrateur, métadonnées pseudonymisées | M | jusqu'à résolution + audit | Point unique de reprise; résolution manuelle/idempotente. |
| `catalog_admin_mutations` | Tenant-owned / acteur admin | M | fenêtre d'idempotence + audit | Hash opération/payload et réponse durable; réemploi contradictoire refusé. |
| `audit_events` | Tenant-owned / journal privilégié | H | politique tenant/légale | Append-only; acteur, membership, action, cible, raison et request ID. |
| `platform_audit_events` | Global / journal control plane | M | politique plateforme/légale | Append-only; acteur plateforme, action, cible, raison, request ID, résultat et trace. |
| `plans` | Global / control plane | XS | vie commerciale + audit | Aucun tenant implicite; affectation via subscription. |
| `tenant_subscriptions`, `tenant_entitlements` | Tenant-owned / billing | S | contrat + audit | Provisionnement atomique depuis le plan et versions de quota. |
| `usage_events`, `usage_rollups` | Tenant-owned / mesure et dérivé | H/M | période billing + audit | Source append-only idempotente; rollup reproductible. |
| `billing_provider_events` | Tenant-owned / événement fournisseur | M | audit commercial | Signature avant routage, hash de payload et déduplication stricte. |
| `outbox_events`, `async_jobs` | Tenant-owned / asynchrone | H | TTL/DLQ + audit | Idempotence tenant, lease, heartbeat, retries bornés et DLQ. |
| `tenant_integrations`, `integration_deliveries` | Tenant-owned / egress | M/H | contrat + historique borné | Endpoint allowlisté, secret chiffré/versionné, signature et tentative durable. |
| `tenant_domains`, `tenant_feature_flags` | Global route / tenant control | S | vie tenant + audit | Seul un domaine vérifié d'un tenant actif route; flags versionnés. |
| `tenant_retention_policies`, `tenant_restore_manifests` | Tenant-owned / conformité | S/M | politique et preuves | Jobs reprenables, checksums DB/objets et validation d'isolation. |

## Objets hors base

| Objet | Classe / propriétaire | Rétention et migration |
|---|---|---|
| DB SQLite, fichiers `-wal`/`-shm` | Conteneur mono-instance de toutes classes | Sauvegarde cohérente; interdit comme backend SaaS multi-instance. Migration vers PostgreSQL contrôlée. |
| `learners/<learner>/MEMORY.md` | Tenant-owned / enrollment | Import vers `narrative_objects` avec tenant/enrollment, checksum et quarantaine des chemins non canoniques. |
| `learners/<learner>/MEMORY_pending.md` | Tenant-owned / enrollment | Même règle; rétention plus courte tant que non consolidé. |
| `learners/<learner>/sessions/*.md` | Tenant-owned / enrollment/session | Import idempotent; timestamp/path non interprétable quarantainé. |
| `learners/<learner>/archives/*.md` | Tenant-owned / enrollment | Conservation selon politique tenant; période invalide quarantainée. |
| `learners/<learner>/concepts/*.md` | Tenant-owned / enrollment/concept legacy | Concept ambigu entre domaines quarantainé. |
| `learners/<learner>/domains/<domain>/concepts/*.md` | Tenant-owned / enrollment/concept | Learner/domain validés avant import. |
| Logs/traces applicatifs | Dérivée / plateforme, tenant pseudonymisé | Pas de secret ni contenu pédagogique; TTL opérationnel; tenant sous forme de label borné/pseudonymisé. |
| Secrets d'environnement et keyrings | Global ou tenant-scoped selon intégration | Jamais en DB/log/URL; rotation versionnée, chiffrement et accès minimal. |
| Sauvegardes et exports | Héritent de chaque ligne/objet | Chiffrés, manifestes tenant-aware, expiration et preuve de restauration/effacement. |

## Relations à auditer avant le contract

Les couples suivants sont contrôlés en SQL avant chaque validation de FK :

- learner ↔ domain : états conceptuels, sessions, interactions, évaluations,
  snapshots, calibrations, transferts, intentions et notifications ;
- learner ↔ session : interactions, états affectifs, évaluations et intentions ;
- learner/domain ↔ curriculum concept/version/metadata ;
- session/domain/concept ↔ assessment attempt ↔ interaction/transfer ;
- réservation `scheduled_alerts` ↔ queue ↔ transition de livraison ;
- objet narratif ↔ mutation ; job de rétention ↔ phase/hold ;
- membership ↔ code/consent/refresh/account challenge ↔ client OAuth.

Le gate de contract exige zéro anomalie non résolue et zéro ligne tenant-owned
avec `tenant_id IS NULL`; le rapport de quarantaine reste exportable pour revue.
