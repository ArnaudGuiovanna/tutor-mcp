# API de lecture du client natif

`GET /api/client/v1/overview` fournit les données structurées de l’interface,
indépendamment de la conversation ACP. Les outils de l’agent restent sur `/mcp`.

L’API partage la ressource OAuth du service Tutor : le jeton conserve son audience
`<URL du service>/mcp`. Elle exige `learner:read`, également couvert par le grant
historique `learner`. Un grant `learner:write`, `formation:read` ou administratif
seul ne donne pas accès à cette vue. La console conserve son authentification
navigateur indépendante.

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

Les paramètres inconnus, répétés, mal encodés ou hors limites sont refusés avec
HTTP 400. Les curseurs des deux listes sont indépendants. Les méthodes autres que
GET sont refusées avec HTTP 405.

La réponse annonce `contractVersion: 1` et contient :

- `identity` : `tenantId`, `userId`, `membershipId`, `learnerId` ;
- `context` : profil déclaré filtré, objectif, domaines, état des concepts et
  compteurs d’interactions du jour, uniquement pour l’apprenant authentifié ;
- `formations` : page des inscriptions et admissions de l’apprenant ;
- `availableFormations` : page du catalogue accessible, avec ses propres
  inscriptions seulement ;
- `capabilities.commonCatalog` : disponibilité du port de lecture du catalogue.

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
