# Incident de latence ACP — 4 novembre 2026

## Périmètre et sources

Analyse du dépôt `5ffeab8` et des journaux du processus MCP réellement exposé
par Tailscale Funnel vers le port local 3000. Le client déclaré est Tutor ACP
0.0.12, avec Codex, GPT-6.1 sol, réflexion élevée et vitesse équilibrée.
La demande est une reprise de formation. Aucun contenu d'apprenant, secret,
identifiant de compte ni adresse de déploiement n'est reproduit ici.

L'heure de 16:02:22 Paris fournie ensuite correspond à une vérification, pas
au début de la séquence retrouvée. Les neuf appels de 14:18:52 à 14:20:27 UTC
(15:18:52 à 15:20:27 Paris) correspondent exactement aux six outils et aux
répétitions décrits. Leur rattachement est fondé sur cette concordance ; les
logs ne portent pas d'identifiant de tour ACP partagé. En revanche, chaque
ligne d'outil est reliée à sa requête HTTP par son `request_id` serveur.

## Mesures retrouvées

Les horodatages ci-dessous sont les fins d'exécution. Les intervalles utilisent
le début estimé de l'appel suivant (`fin - duration_ms`), avec la précision
milliseconde des logs. HTTP inclut l'outil : les deux colonnes ne s'additionnent
pas. Le transport hors serveur n'est pas mesuré par ces durées.

| Fin UTC | Outil | Résultat | Outil (ms) | HTTP (ms) | Intervalle depuis l'outil précédent (s) |
|---|---|---|---:|---:|---:|
| 14:18:52.342 | `list_training_library` | succès | 29 | 46 | — |
| 14:18:52.411 | `get_conversation_context` | succès | 3 | 14 | 0,066 |
| 14:18:56.109 | `select_conversation_training` | succès | 29 | 54 | 3,669 |
| 14:18:58.508 | `get_curriculum_snapshot` | succès | 6 | 20 | 2,393 |
| 14:19:02.801 | `get_next_activity` | succès | 91 | 124 | 4,202 |
| 14:19:31.310 | `prepare_assessment_attempt` | refus | 11 | 27 | 28,498 |
| 14:19:41.720 | `get_curriculum_snapshot` | succès | 16 | 52 | 10,394 |
| 14:19:59.927 | `prepare_assessment_attempt` | refus | 7 | 33 | 18,200 |
| 14:20:27.250 | `prepare_assessment_attempt` | succès | 51 | 71 | 27,272 |

Les neuf appels cumulent **243 ms d'exécution d'outil**, inclus dans **441 ms
de traitement HTTP**. Du début estimé du premier outil à la fin du dernier,
il s'écoule **94,937 s**. Les deux refus consomment 18 ms d'outil et 60 ms HTTP.
Les délais visibles dans le client incluent d'autres portions du parcours :
ils ne sont donc pas interchangeables avec ces mesures serveur.

Le relevé utilisateur mentionne ensuite onze appels techniques totalisant
environ 1,1 s. Les neuf appels MCP ci-dessus ne prouvent pas la durée ni
l'identité des deux autres. Ils ne permettent pas non plus de retrouver
l'instant d'envoi ni celui de l'affichage final. On ne soustrait donc pas
243 ms ou 441 ms des 105 s pour annoncer un « temps modèle » exact.

## Causes établies et limites d'attribution

1. **Le serveur en service n'avait pas été mis à jour.** Le binaire annonce
   `dev`, commit `100cb8def6f1c11dada29e48027f0ae1648720c5`, compilation le
   26 octobre à 16:06:49 UTC. Son empreinte correspond au reçu local du
   déploiement de cette date. Le processus est démarré depuis le 28 octobre.
   Le binaire ne contient pas `prepare_pedagogical_turn` ; le ledger s'arrête
   à `postgres_0074_conversation_training`. Publier v0.7.4 sur GitHub n'avait
   pas remplacé le binaire du service.
2. **Le contexte regroupé était donc indisponible.** En l'absence de cet
   outil dans le catalogue, `Backend::prepared_turn` retourne `None` et
   conserve le parcours compatible. Le harness orchestre alors les lectures,
   la sélection et la préparation. Les attentes longues observées sont entre
   ces appels courts, pas dans une requête MCP de 105 s.
3. **Une incompatibilité d'identifiants est reproduite.** Le curriculum
   génère des IDs opaques en base64url, comportant potentiellement des
   majuscules et tirets. Le validateur de barème lié à une décision exigeait
   leur égalité avec une forme passée en minuscules et séparée par underscores.
   Un ID valide du curriculum pouvait donc être refusé. Cela correspond au
   récit fourni sur les deux refus ; leurs paramètres et messages d'erreur
   exacts ne sont pas présents dans les logs d'exploitation.
4. **La séparation modèle/transport/affichage reste incomplète.** Les
   intervalles de 28,498 s, 18,200 s et 27,272 s peuvent inclure génération,
   orchestration, transport et attente côté client. Les logs serveur seuls
   ne permettent pas de les attribuer exclusivement au fournisseur.

## Correction et conservation des garanties

Le validateur commun accepte les identifiants opaques sans transformer leur
casse ni leurs tirets. La comparaison pendant la notation reste exacte.
Les doublons, critères étrangers au barème, scores incohérents, absence de
preuve et JSON ambigu restent rejetés. Aucun score historique n'est réécrit
et aucune confiance supplémentaire n'est accordée à une évaluation `host_llm`.

La régression reproduit d'abord le rejet, puis vérifie les IDs générés par le
curriculum, leur conservation lors du gel et de la notation, ainsi que le
rejet d'un ID différent. Le parcours MCP vérifie aussi le rejeu idempotent de
la préparation, la soumission préalable et l'absence de confiance inventée,
avec SQLite et PostgreSQL.

La remise à niveau du déploiement doit conserver le binaire précédent, une
sauvegarde récupérable, les clés et les données. Elle applique la migration
additive de maintenance déjà livrée dans v0.7.4, avec les rôles existants,
puis vérifie `/ready`, le Funnel et la révision effective. Une release publiée
ou une CI verte ne remplace aucune de ces vérifications opérationnelles.

## Ce qui reste nécessaire pour l'objectif de cinq secondes

**L'objectif de réponse complète en cinq secondes n'est pas qualifié par cet
audit.** Même après mise à jour, une reprise en texte libre n'est pas la demande
structurée quiz/flashcard : elle prépare un contexte, mais peut encore nécessiter
une sélection et une décision supplémentaires. Le bouton Reprendre délègue
encore cette sélection au harness sous forme de texte. La mise à jour est
nécessaire, mais ne prouve pas à elle seule que tous les parcours sont regroupés.

La suite doit mesurer la version effectivement déployée et corréler, pour un
même tour, préparation native, chargement de session ACP, permissions,
appels du harness, génération complète, finalisation et affichage. Rejouer
reprise, explication et réponse d'exercice à froid puis à chaud, avec les
réglages déclarés, et publier les dépassements avec les succès.

Le chemin visé reste : sélection explicite et décision côté serveur, paquet
pédagogique complet et borné, une formulation par le harness, puis gel et
persistance côté client/serveur. Il faut couvrir la reprise générique sans la
transformer arbitrairement en quiz ni noter une demande d'aide comme réponse.
Réduire les aller-retours évite des attentes cumulatives ; cela ne borne pas
le temps d'une génération externe. Un plafond absolu de cinq secondes exige
des réponses pédagogiques déjà préparées et encore valides pour les parcours
qui s'y prêtent, ou une modification explicite du contrat de service. Un
timeout, un premier fragment ou un message d'attente ne valent pas une réponse
complète réussie.
