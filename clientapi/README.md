# API de lecture du client natif

`GET /api/client/v1/overview` fournit les données structurées de l’interface,
indépendamment de la conversation ACP. Les outils de l’agent restent sur `/mcp`.

L’API partage la ressource du service Tutor : le jeton conserve son audience
`<URL du service>/mcp`. En mode institution, il provient d’une session native
ouverte par preuve de possession de la clé, et reste vérifié en base à chaque
appel. Le contexte personnel exige `learner:read` et une adhésion apprenant.
Un responsable disposant de `formation:read` et de la permission de conception
reçoit sa propre identité et ses capacités, sans profil apprenant artificiel.
Le droit `formation:write` reste une délégation séparée et explicite.

Les identifiants d’institution, d’utilisateur, d’adhésion et d’apprenant proviennent
du principal signé et de son état courant en base. Le principal est revérifié dans
la transaction de lecture liée à l’institution. Aucune sélection d’apprenant ou
d’institution par paramètre n’est admise. Les limites IP, principal et concurrence
sont les mêmes instances que celles du transport MCP. Toutes les réponses portent
`Cache-Control: no-store`, y compris les refus d’authentification.

Paramètres facultatifs :

| Paramètre | Valeur |
|---|---|
| `limit` | Taille de chaque page de formations, de 1 à 100 ; défaut 20 |
| `after` | `formations.next_after` reçu lors de la page précédente |
| `available_after` | `availableFormations.next_after` reçu précédemment |
| `authored_after` | `authoredFormations.next_after` reçu précédemment |

Les paramètres inconnus, répétés, mal encodés ou hors limites sont refusés avec
HTTP 400. Les curseurs des trois listes sont indépendants. Les méthodes autres que
GET sont refusées avec HTTP 405.

La réponse annonce `contractVersion: 1` et contient :

- `identity` : `tenantId`, `userId`, `membershipId`, `learnerId` et `roles` ;
- `context` : profil déclaré filtré, objectif, domaines, état des concepts et
  compteurs d’interactions du jour, uniquement pour l’apprenant authentifié ;
- `formations` : page des inscriptions et admissions de l’apprenant ;
- `availableFormations` : page du catalogue accessible, avec ses propres
  inscriptions seulement ;
- `authoredFormations` : brouillons et formations publiées visibles par le
  responsable, suivant ses droits de propriété ou ses affectations ;
- `capabilities.commonCatalog` : disponibilité du port de lecture du catalogue ;
- `capabilities.learning` : disponibilité du contexte personnel ;
- `capabilities.formationAuthoring` : permission de conception et délégation
  d’écriture effectivement présentes. Un rôle propriétaire seul ne suffit pas.

Pour une identité responsable sans rôle apprenant, `learnerId` et l’identifiant
du contexte sont vides ; les listes personnelles restent vides. Le client utilise
alors les outils MCP de conception, sans appeler un outil réservé aux apprenants.

Les pages de formations conservent les noms de champs existants en `snake_case`,
notamment `next_after`, `cohort_id` et `enrollment.domain_id`. Les nouveaux objets
de cette API utilisent `camelCase`.

Le modèle de lecture actuel n’expose pas de pseudonyme : le client ne doit pas
inventer de nom. Il n’expose pas non plus la politique institutionnelle de création
de domaines personnels : `capabilities.personalLearning` reste absent tant que
cette politique ne peut pas être déterminée. Une absence indique une capacité
inconnue et ne constitue pas une autorisation. Les mutations restent soumises aux
contrôles serveur des outils MCP.

Cette projection exclut email, mot de passe, données d’intégration, notes libres
du profil et réponses brutes aux activités. Les choix de projection sont testés
sur SQLite et sur un rôle PostgreSQL sans propriété des tables ni contournement
RLS, via `TestClientAPI*`.


## Conversation et bibliothèque unifiée (client 0.2)

`POST /api/client/v1/training/{action}` accepte un objet `TrainingRequest`.
Les actions correspondent aux outils MCP de même nom. Les lecteurs exigent
`learner:read`, `formation:read` ou `formation:contribute`, les écritures personnelles `learner:write`. Les contributions, le partage,
les décisions et les réglages communs exigent la délégation distincte
`formation:contribute` en plus des capacités de la ressource. Le bundle historique
`learner` n’accorde jamais cette nouvelle délégation. `get_training_groups` exige `progress:read` et les affectations
réelles. Aucune permission administrative n’est accordée aux contributeurs.
Toutes les opérations revérifient le principal dans une transaction d’institution.

- `list_training_library` : `query`, `filter`, `after`, `limit` ; déduplication,
  confidentialité et pagination côté serveur. `next_after` poursuit la même recherche.
- `get_training` : `id` réel `personal:<domain_id>` ou `formation:<formation_id>` ;
  programme, progression privée, révision, capacités, calibration et proposition.
- `get_conversation_context`, `select_conversation_training` : contexte associé à
  l’adhésion et à `conversation_id`, sélection avec `expected_selection`.
- `preview_training_edit`, `propose_training_edit`, `save_training_edit` : document
  complet et `expected_revision` ; les conséquences pédagogiques proviennent du
  moteur existant. Un programme commun publié crée la version suivante.
- `share_training`, `submit_training`, `decide_training`, `publish_training` :
  copie de conception, candidature figée, décision motivée, publication atomique.
- `set_training_access`, `find_training_members` : destinataires et éditeurs par
  nom ou clé publique ; seul le propriétaire autorise la publication libre.
- `get_training_policy`, `set_training_policy`, `join_training` : politique
  personnelle explicite et inscription contrôlée.

Chaque écriture exige `key`, propre à l’action, réutilisée uniquement pour une
reprise exacte. Les reçus sont liés à l’auteur et l’institution. HTTP 409 signale
une révision obsolète ou une clé réutilisée avec un contenu différent ; 404 ne
révèle pas l’existence d’une ressource inaccessible. Les paramètres inconnus et
les corps multiples sont refusés. Les réponses restent `no-store`.

Le transport MCP natif porte `Tutor-Conversation-ID` (HTTP) ou
`TUTOR_CONVERSATION_ID` (stdio). Les outils de routage et d’observation refusent
un autre domaine, y compris un identifiant explicite. Les clients MCP historiques
sans contexte conservent leur contrat existant.
