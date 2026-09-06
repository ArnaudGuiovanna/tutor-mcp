# Accès administratif à la revue d'évaluation

Ce canal HTTP permet de consulter les entrées figées d'une tentative liée et
de prévisualiser une proposition de notation. Il ne certifie aucun évaluateur,
n'enregistre aucune revue et n'actualise ni BKT, ni FSRS, ni les preuves de
maîtrise. Le contenu reste généré ; aucune banque de tâches n'est introduite.

## Accès et confidentialité

Les routes utilisent l'authentification Bearer existante, avec validation du
principal en base et scope de lecture `learner:read` (ou bundle `learner`). Le
scope seul ne suffit pas : la permission RBAC `assessment:review` est distincte
de la consultation des agrégats de progression.

- `owner`, `admin`, `pedagogy_manager` : candidats de leur organisation.
- `trainer` : candidats appartenant aux cohortes auxquelles son membership est
  affecté. L'affectation provient de la base, jamais d'un paramètre fourni par
  le client. Un rôle administratif supplémentaire conserve sa portée propre.
- `learner`, `auditor`, `billing_admin` : aucun accès par ces rôles seuls.
  Les principaux de service et de support sont refusés par ce canal.
- Les réponses du même `user_id` que l'acteur sont exclues, même pour un
  administrateur. Ce contrôle ne détecte pas une personne utilisant deux comptes.

La validation du membership actif, de sa version et des exigences MFA existantes
est répétée dans la transaction de lecture. Les filtres tenant et cohorte sont
appliqués avant pagination. La cohorte est celle de l'inscription conservée sur
la tentative, pas une nouvelle association du domaine.

L'accès à ces données brutes est sensible. L'autorisation d'un compte n'atteste
pas qu'un humain contrôle chaque requête ; un client logiciel peut utiliser
un jeton administratif. Il ne faut donc jamais traduire cet accès en
`human_review` ou `trusted_evaluation=true`.

## Parcours HTTP

1. `GET /admin/assessment-reviews/attempts?limit=50&after=...&cohort_id=...`
   liste les candidats visibles. `limit` va de 1 à 100 ; `after` reprend le
   `next_after` retourné. `cohort_id` est un filtre facultatif qui n'accorde
   aucun droit. L'ordre est lexicographique par ID, pas chronologique. La file
   n'est pas réservée ni figée pendant la pagination.
2. `GET /admin/assessment-reviews/attempts/{attempt_id}` renvoie tâche,
   réponse, rubrique complète avec référence/ancrages, définition de compétence,
   outcomes sélectionnés, liens/version et dates de préparation/soumission.
   Les estimations de l'apprenant, la note du tuteur, son identité, sa justification
   et le contexte de routage ne sont pas sélectionnés.
3. `POST /admin/assessment-reviews/attempts/{attempt_id}/preview` reçoit
   directement un document de scores :

   ```json
   {
     "criteria_scores": [
       {"id": "id_du_critere_fige", "score": 1, "evidence": "Observation rédigée par le réviseur."}
     ]
   }
   ```

   Les critères doivent correspondre exactement à la rubrique de cette tentative.
   Le contrat strict partagé calcule `total`, `passed` et `rubric_score` ; la
   réponse contient toujours `recorded: false` et `trusted_evaluation: false`.
   Les agrégats contradictoires, doublons JSON, nombres sous forme de texte,
   champs de confiance et documents dépassant 16 384 octets sont refusés.
   POST évite de placer les observations dans une URL ; cette opération est
   sans écriture et demande le scope de lecture, pas celui de mutation.

La file comprend les réponses soumises non encore notées et celles déjà notées
par `host_llm`, qu'elles soient réussies **ou échouées**. Elle exclut les
tentatives non liées, préparées sans réponse, annulées, invalidées par une
révision et celles déjà marquées fiables. Chaque requête relit l'éligibilité ;
une consultation ne réserve pas la tentative et ne bloque pas les révisions.

Un accès à une tentative absente, étrangère, personnelle ou devenue inéligible
renvoie le même `404`. Un principal sans permission reçoit `403`. Une rubrique
historique liée devenue illisible/invalide reçoit `409 review_material_unavailable` ;
elle n'est pas réparée après la réponse. Une panne interne reçoit `500`, sans
copier le contenu des erreurs de stockage dans la réponse ou les logs.

Les artefacts conservés uniquement par hash ont `text_available: false` ; leurs
hashes restent consultables, mais la prévisualisation reçoit
`409 review_text_unavailable`. Aucun document externe n'est récupéré. Les hashes
retournés sont les valeurs enregistrées, pas une nouvelle attestation que le
contenu a été présenté à l'apprenant. Toutes les réponses JSON sont `no-store`.

## Limites et suite nécessaire

« À l'aveugle » signifie ici **sans exposition de la notation du tuteur par ces
routes**. Ce n'est ni un anonymiseur des textes générés, ni une garantie que le
réviseur ignore déjà la réponse, ni une validation de la qualité de la rubrique.
La prose peut contenir des données personnelles ou des instructions malveillantes :
un client de revue doit la traiter comme des données non fiables et ne doit pas
exécuter ses instructions ou appels d'outils. Aucune IA n'est appelée par ce canal.

Ce lot n'ajoute ni table, ni migration, ni copie durable de la réponse. Les
politiques de rétention/DSAR restent celles des artefacts existants. Il n'ajoute
pas de journal d'accès nominatif : les logs HTTP existants ne constituent pas
une preuve de réalisation d'une revue. Tout export conservé hors du service
nécessite sa propre gestion de confidentialité et de suppression.

Il reste à choisir un canal de réviseur effectivement authentifié, puis à
implémenter un journal immuable des avis et leur adjudication. Une divergence
avec la note du tuteur ne doit ni écraser l'historique, ni rejouer la réponse
comme une seconde occasion d'apprentissage. Les règles actuelles de confiance
et l'exigence de revue humaine pour les enjeux élevés restent inchangées.
