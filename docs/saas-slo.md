# SLO du MVP SaaS

Ces objectifs sont les budgets d'exploitation initiaux du profil PostgreSQL.
Ils doivent être recalibrés après 30 jours de trafic représentatif ; les
mesures synthétiques de CI ne valent pas engagement client.

| Indicateur mensuel | Objectif | Mesure |
|---|---:|---|
| Disponibilité API/MCP | 99,9 % | requêtes authentifiées hors 4xx client avec réponse non-5xx |
| Latence MCP hors LLM | p95 < 2 s, p99 < 5 s | `tutor.mcp.tool.duration` par outil et résultat |
| Prise en charge d'un job | 99 % < 5 min, 99,9 % < 30 min | `tutor.queue.lag` à l'instant du claim |
| Exécution worker | 99,5 % sans échec interne | `tutor.worker.runs{job.outcome}` |
| Livraison webhook acceptée | 99 % < 15 min hors 4xx destinataire | transitions queue + `integration_deliveries` |
| Fraîcheur d'archivage WAL (RPO) | < 5 min | âge du dernier WAL archivé et alerte archiveur |
| Restauration PostgreSQL (RTO) | < 30 min pour le dataset MVP mesuré | rapport horodaté de l'exercice PITR/full restore |

Le budget d'erreur 99,9 % est d'environ 43 min 50 s sur 30 jours. Une erreur
RLS refusée est un succès de sécurité si la requête est illégitime ; une erreur
RLS sur une requête légitime ou toute fuite A/B consomme immédiatement tout le
budget et bloque le rollout.

## Alertes minimales

| Signal | Warning | Critical / action |
|---|---|---|
| `/ready` | 1 échec sur 3 sondes | 3 échecs ; retirer l'instance du trafic |
| taux HTTP/MCP 5xx | > 1 % pendant 5 min | > 5 % pendant 5 min ; geler le rollout |
| MCP p95/p99 | p95 > 1,5 s | p95 > 2 s ou p99 > 5 s pendant 10 min |
| pool SQL | `in_use/max_open` > 80 % | > 95 % ou attente croissante pendant 5 min |
| queue lag | > 5 min | > 30 min ; vérifier worker, quota et DB |
| DLQ | première ligne | croissance sur deux passages ; intervention requise |
| run worker échoué | > 1 % sur 15 min | même tenant/job sur 3 runs ou > 5 % fleet-wide |
| archive WAL | aucun succès depuis 3 min | âge > 5 min ou `failed_count` augmente |
| audit/RLS | erreur légitime isolée | suspicion cross-tenant : couper le canary |

Les dashboards doivent séparer résultat `denied` de `failed`, afficher les
quantiles par nom borné d'outil/job, le tenant pseudonymisé seulement pour le
diagnostic, et conserver une vue fleet-wide sans tenant. Ne jamais utiliser
membership, enrollment, `event_id` ou trace ID comme label de métrique. Ils
appartiennent aux spans/journaux consultés à la demande.

Chaque `tools/call` entrant dans le SDK produit une seule observation, y compris
un replay idempotent, une erreur de réservation, de transaction ou de commit.
La durée comprend l'attente SQL et se termine après le résultat final et
l'application de la deadline serveur. Un timeout est `failed`, même si le
handler a renvoyé tardivement un succès ou un refus. Les refus OAuth/RBAC sont
`denied` ; le refus OAuth HTTP 403 avant le SDK est compté à cette frontière.
Les noms d'outils absents du registre de cette instance partagent le label
`unknown`. Les autres méthodes MCP et les refus HTTP d'authentification ou de
limitation avant cette frontière ne sont pas des appels d'outil observés.

## Gate HTTP synthétique

`TestMCPHTTPConcurrentIdentityAndReplay` vérifie toujours les identités A/B,
les objectifs durables, les refus de domaine étranger et le replay exact via
le véritable assemblage HTTP utilisé par `main`. Les jobs SQLite et PostgreSQL
exécutent cette preuve de correction avec le détecteur de races. Le gate de
latence est opt-in et s'exécute ensuite **sans race**, séparément par backend :

```sh
MCP_HTTP_PERF_BUDGET=1 go test -v -count=1 -timeout 3m . -run '^TestMCPHTTPPerformanceBudget$/^sqlite$'
TUTOR_TEST_PG_DSN='postgres://…' MCP_HTTP_PERF_BUDGET=1 go test -v -count=1 -timeout 3m . -run '^TestMCPHTTPPerformanceBudget$/^postgres$'
```

La fixture PostgreSQL crée un schéma privé via un compte de migration, puis
sert toutes les requêtes MCP avec un rôle non propriétaire, sans SUPERUSER,
BYPASSRLS ni CREATE. Les dix connexions runtime appliquent ce rôle à
l'ouverture. Des lectures positives sous chaque tenant accompagnent les
lectures non filtrées négatives RLS. SQLite utilise un fichier WAL privé,
un writer et quatre readers. Le backend narratif partage exactement le Store
runtime des outils ; aucune requête HTTP n'utilise le Store administratif.

Le jeu contient 50 learners répartis sur deux institutions, chacun avec
12 états de concepts, 24 interactions historiques et 32 récits chiffrés. La
préparation, la vérification des enveloppes et des lectures propriétaires,
l'initialisation des 50 sessions MCP et quatre séquences de warmup (16 appels)
restent hors mesure. Quatre workers traitent une séquence à la fois par learner,
après une barrière de départ. Les **200 appels mesurés au total** comprennent
50 appels de chacun des outils `get_learner_context`, `get_pending_alerts`,
`get_next_activity` et `update_learner_profile`. Les mutations emploient des
clés fraîches ; le harness ne retente ni ne rejoue les appels mesurés.

Chaque durée couvre `CallTool`, l'authentification, les attentes SQL, le commit
et le décodage de la réponse. Les réponses doivent réussir au niveau MCP et
métier : identité et objectif propres, contexte initialisé, trois récits exacts
du domaine attendu sans dégradation, nouvelle décision liée à une session
cohérente éventuellement réutilisée, puis objectif, session et décision relus
durablement hors chrono. Les quantiles
sont calculés **par outil**, par rang supérieur : avec 50 échantillons, p99 est
le maximum observé. Les budgets initiaux sont p95 < 2 s et p99 < 5 s.

Un cas séparé ajoute ensuite 1024 récits d'un autre domaine, tous plus récents
que les 32 récits utiles du même learner (1056 objets, sous le quota de 2048).
Un nouvel appel d'activité doit encore rendre les trois récits utiles exacts,
sans dégradation, et persister une nouvelle décision. Cette observation unique
doit durer moins de 5 s ; elle n'est ni incorporée au p95 global ni présentée
comme un percentile.

Le journal rapporte volumes, latences, durée du sweep et débit effectivement
observé, sans seuil de débit minimal. Les deltas `WaitCount`/`WaitDuration`
concernent le pool **writer** SQLite ou **shared** PostgreSQL ; les attentes
du reader SQLite ne sont pas échantillonnées par ce harness. Les deux limiteurs
de fixture, **IP et apprenant**, sont relevés chacun à **1000 jetons/seconde et
un burst de 2000 jetons**, pour accueillir l'initialisation et les appels depuis
le loopback. Les middlewares restent actifs et les plafonds de concurrence
production 128/8 sont conservés. Le watchdog initial du gate est de trois minutes.

Deux exécutions locales avec Go 1.26.9 ont passé le gate : un run SQLite le
15 octobre 2026 à 18:47 UTC et un run PostgreSQL le 16 octobre 2026 à 07:55 UTC.
La préparation et le warmup étaient hors chrono. Les latences ci-dessous sont
en millisecondes arrondies, avec 50 échantillons par outil et par backend ; p99
est donc le maximum observé, et non une estimation précise de la queue de
distribution en production.

| Outil | SQLite p95 | SQLite p99 | PostgreSQL p95 | PostgreSQL p99 |
|---|---:|---:|---:|---:|
| `get_learner_context` | 55 | 63 | 52 | 79 |
| `get_pending_alerts` | 40 | 49 | 43 | 51 |
| `get_next_activity` | 143 | 165 | 151 | 211 |
| `update_learner_profile` | 104 | 120 | 29 | 30 |

Le sweep de 200 appels a duré 2,142 s sur SQLite (93,35 appels/s observés) et
1,726 s sur PostgreSQL (115,86 appels/s observés). L'observation séparée avec
1024 récits étrangers a duré respectivement 23,59 ms et 64,71 ms. Ces deux runs
à des dates différentes ne constituent pas une comparaison contrôlée des
backends, un seuil de débit garanti ou une mesure de capacité maximale.

Le test `tools/TestGetPendingAlertsThenNextActivityP95Budget` demeure un
microbenchmark en mémoire de deux handlers : il n'inclut pas la chaîne HTTP,
les transactions de production ni les récits de sessions chiffrés. Le gate
HTTP mesure un scénario borné sur un runner ; il ne démontre ni capacité
maximale, ni livraison réseau des workers, ni engagement client. Le gate DB
de réservations avec noisy neighbour reste une preuve distincte.

## Exercices et revue

- chaque déploiement : smoke `/live`/`/ready`, OAuth négatif, appel MCP canary,
  outbox→worker→webhook de test et absence de DLQ ;
- mensuel : perte forcée d'un worker, expiration/reclaim d'un lease et noisy
  neighbour gros/petit tenant ;
- trimestriel : restauration complète chiffrée, PITR à une cible et
  restauration logique tenant dans une base isolée ;
- après incident ou changement de schéma/keyring : répéter l'exercice concerné.

Le rapport conserve version/image PostgreSQL, LSN ou backup ID, latence
d'archivage, RTO, checksums et décision. Le 2026-08-12, l'exercice synthétique
PostgreSQL 17 du dépôt a restauré le LSN cible en 1 908 ms, avec WAL disponible
en 553 ms ; le marqueur antérieur était présent et le marqueur postérieur
absent. Ces nombres prouvent le mécanisme, pas la capacité production.
