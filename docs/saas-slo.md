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
