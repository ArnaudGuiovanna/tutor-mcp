# Accès administratif à la revue d'évaluation

Ce canal HTTP permet de consulter les entrées figées d'une tentative liée et
de prévisualiser une proposition de notation et d'enregistrer un avis authentifié.
Il ne certifie aucun évaluateur et n'actualise ni BKT, ni FSRS, ni les preuves de
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
est répétée dans la transaction de lecture ou d'écriture. Les filtres tenant et cohorte sont
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
   `material_hash` et l'en-tête `ETag` identifient cette projection figée.
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
4. `POST /admin/assessment-reviews/attempts/{attempt_id}/reviews` reçoit le
   **même document de scores**, avec les scopes `learner:read` **et** `learner:write`
   (ou bundle `learner`). Deux en-têtes sont obligatoires :

   ```http
   Idempotency-Key: une-cle-unique-pour-cet-avis
   If-Match: "valeur-de-material_hash"
   ```

   Le serveur recalcule le score sur la rubrique figée et conserve l'identité
   issue du principal validé, sa version de jeton, la date serveur, les hashes
   du matériel et du score canonique. Aucun champ `reviewer_user_id`,
   `evaluation_method` ou `trusted_evaluation` n'est accepté dans le score.
   Réponse `201` avec `{review, recorded: true, replayed: false}` ; une relance
   identique reçoit `200`, le même avis et `replayed: true`. L'avis contient
   toujours `trusted_evaluation: false` : le compte n'est pas une attestation
   d'exécution humaine, ni de correction sémantique.
5. `GET /admin/assessment-reviews/attempts/{attempt_id}/reviews/mine` restitue
   uniquement l'avis du compte authentifié, jamais ceux des autres réviseurs.
   Les droits restent vérifiés, même après enregistrement. L'avis reste
   consultable après invalidation du curriculum ; le champ
   `curriculum_invalidated_version` signale alors cette invalidation.

Un seul premier avis est conservé par organisation/tentative/`user_id`, même
avec une autre clé. La clé d'idempotence est propre à l'organisation et au compte,
limitée à 128 caractères ; sa réutilisation avec un autre contenu ou une autre
tentative reçoit `409 review_conflict`. Les relances comparent le score canonique,
pas les espaces ou l'ordre des propriétés JSON. Une précondition manquante reçoit
`428`, une empreinte ne correspondant plus au matériel `412`. Seul un ETag fort
unique est accepté, pas `*`, une liste ou un ETag faible.

La transaction verrouille le domaine puis la tentative avant de relire le matériel.
Une révision concurrente est donc ordonnée par rapport à l'avis ; elle ne peut pas
laisser un avis traité comme actuel sur une tentative déjà invalidée. L'audit
`assessment.review.record` est écrit atomiquement, sans recopier la prose.
Ni l'évaluation d'origine ni les modèles d'apprentissage ne sont modifiés.

La file comprend les réponses soumises non encore notées et celles déjà notées
par `host_llm`, qu'elles soient réussies **ou échouées**. Elle exclut les
tentatives non liées, préparées sans réponse, annulées, invalidées par une
révision et celles déjà marquées fiables. Elle exclut aussi les tentatives déjà
revues par le compte, **avant** pagination ; les autres comptes peuvent encore
les examiner à l'aveugle. Chaque requête de matériel/notation relit l'éligibilité ;
une consultation ne réserve pas la tentative et ne bloque pas les révisions.
Une relance d'écriture devenue inéligible est refusée ; `reviews/mine` reste le
point de récupération historique, sous réserve de droits toujours actifs.

Un accès à une tentative absente, étrangère, personnelle ou devenue inéligible
renvoie le même `404`. Un principal sans permission reçoit `403`. Une rubrique
historique liée devenue illisible/invalide reçoit `409 review_material_unavailable` ;
elle n'est pas réparée après la réponse. Une panne interne reçoit `500`, sans
copier le contenu des erreurs de stockage dans la réponse ou les logs.

Les artefacts conservés uniquement par hash ont `text_available: false` ; leurs
hashes restent consultables, mais la prévisualisation reçoit
`409 review_text_unavailable` et l'enregistrement `409 review_material_unavailable`.
Aucun document externe n'est récupéré. Les hashes tâche/réponse restent les valeurs
enregistrées. `material_hash`, lui, est un SHA-256 versionné calculé sur la projection
réellement lue (dates UTC et champ `material_hash` vide dans le préimage). Il lie
l'avis aux artefacts stockés, **pas** à une preuve qu'ils ont été présentés à
l'apprenant. Toutes les réponses JSON sont `no-store`.

## Stockage et cycle de vie

Les migrations additives SQLite `0066_assessment_reviews` et PostgreSQL
`postgres_0057_assessment_reviews` créent `assessment_reviews`. PostgreSQL impose
la RLS tenant. Les contraintes de portée et d'unicité protègent le journal ; les
triggers interdisent la réécriture d'un avis, sauf la suppression irréversible
de son document de scores pour rétention. Aucun endpoint de modification ou de
suppression d'avis n'est exposé. La réponse apprenant n'est pas dupliquée.

`AssessmentPlaintextDays` couvre aussi le texte des avis à partir de leur date
d'enregistrement, même si le tuteur n'a pas encore noté la tentative. Zéro conserve
les données. La purge respecte les legal holds de l'apprenant évalué et fournit
le compteur `assessment_review_plaintext`. Après purge, `rubric_score` vaut `null` ;
résultat, empreintes et provenance restent présents. Une relance identique ne
restaure pas le texte effacé. L'effacement DSAR de l'apprenant supprime ses avis
avant ses tentatives, y compris pour les demandes ouvertes avant cette migration.
Le manifeste d'export DSAR compte désormais les avis ; ce manifeste n'est pas
un export intégral des textes. La provenance professionnelle du réviseur reste
attachée au dossier de l'apprenant évalué, comme celle de l'audit.

Au déploiement, appliquer les migrations avec le migrateur puis réappliquer
`deploy/postgres-roles.sql` : le worker reçoit lecture/suppression des avis et
uniquement la mise à jour de `rubric_score_json` pour leur purge, pas leur insertion.

## Limites et suite nécessaire

« À l'aveugle » signifie ici **sans exposition de la notation du tuteur par ces
routes**. Ce n'est ni un anonymiseur des textes générés, ni une garantie que le
réviseur ignore déjà la réponse, ni une validation de la qualité de la rubrique.
La prose peut contenir des données personnelles ou des instructions malveillantes :
un client de revue doit la traiter comme des données non fiables et ne doit pas
exécuter ses instructions ou appels d'outils. Aucune IA n'est appelée par ce canal.

L'audit atteste l'enregistrement d'un avis par un compte, pas la lecture effective
du matériel ni une vérification humaine. Tout export conservé hors du service
nécessite sa propre gestion de confidentialité et de suppression.

Il reste à brancher une autorité indépendante de certification (service distinct
ou processus humain vérifiable), puis à implémenter l'adjudication. Une divergence
avec la note du tuteur ne doit ni écraser l'historique, ni rejouer la réponse
comme une seconde occasion d'apprentissage. Les règles actuelles de confiance
et l'exigence de revue humaine pour les enjeux élevés restent inchangées.
