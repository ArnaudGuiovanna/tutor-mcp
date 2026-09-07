# Certification externe et arbitrage

Le serveur peut vérifier une attestation **signée par une autorité configurée
hors du canal du tuteur**, puis conserver une décision d'arbitrage versionnée.
Sans configuration d'autorité, aucun certificat n'est accepté. Le serveur ne
possède pas la clé privée et n'appelle pas lui-même un évaluateur.

La signature identifie l'autorité désignée par l'opérateur ; la qualité et
l'indépendance réelle de son processus restent à établir. Le mécanisme accepte
uniquement `external_service`. Une signature ou un jeton administratif ne peut
pas se déclarer `human_review`. Les domaines à enjeux élevés exigent toujours
une revue humaine fiable et n'obtiennent pas cette confiance par ce canal.

## Configurer la frontière

`ASSESSMENT_CERTIFIERS_JSON` contient une liste bornée de clés publiques Ed25519 :

```json
[
  {
    "key_id": "review-service-2026-09",
    "authority_id": "independent-review-service",
    "tenant_id": "tenant-opaque",
    "public_key": "CLE_PUBLIQUE_ED25519_32_OCTETS_EN_BASE64URL_SANS_PADDING"
  }
]
```

Chaque clé est limitée à une organisation exacte. Identifiants dupliqués,
configuration invalide ou clés de mauvaise taille font échouer le démarrage.
Les clés privées demeurent dans le service indépendant. Pour tourner une clé,
déployer les anciennes et nouvelles clés publiques avec des IDs distincts,
puis retirer l'ancienne. Le retrait empêche les nouveaux certificats ; il ne
retire pas automatiquement les anciennes décisions acceptées. En cas de
compromission, examiner les décisions affectées et les remplacer par des
décisions `reject` signées par une autorité encore autorisée.

## Attestation

L'autorité examine les artefacts figés et la proposition de notation. Le
réviseur peut lui transmettre son avis et les matériaux par un canal externe
autorisé ; le serveur n'exporte rien automatiquement. L'autorité signe les
claims suivants, encodés en JSON UTF-8 puis en base64url sans padding :

```json
{
  "id": "identifiant-unique-du-certificat",
  "audience": "https://votre-origine/admin/assessment-reviews",
  "tenant_id": "tenant-opaque",
  "attempt_id": "attempt-opaque",
  "review_id": "review-opaque",
  "material_hash": "SHA256_DU_MATERIEL_64_CARACTERES_HEXADECIMAUX",
  "score_hash": "SHA256_DU_SCORE_CANONIQUE_64_CARACTERES_HEXADECIMAUX",
  "verdict": "accept",
  "expected_revision": 0,
  "issued_at": 1788778800,
  "expires_at": 1788779100
}
```

L'audience est exactement `BASE_URL` sans slash final, suivi de
`/admin/assessment-reviews`. Les dates sont des secondes Unix entières :
émission non future, expiration strictement future, durée maximale 15 minutes.
Les hashes sont ceux de l'avis enregistré et des matériaux lus. L'ID du
certificat assure l'idempotence par organisation et autorité. `expected_revision`
vaut 0 pour la première décision, puis la révision courante attendue.

Le message Ed25519 est l'octet-à-octet suivant, avec deux séparateurs LF :

```text
tutor-assessment-certification-v1\n{key_id}\n{payload_base64url}
```

Les séquences `\n` ci-dessus représentent des sauts de ligne, sans saut final.
Le corps HTTP est l'enveloppe :

```json
{"key_id":"review-service-2026-09","payload":"BASE64URL_DU_JSON","signature":"BASE64URL_SIGNATURE_ED25519"}
```

L'algorithme est fixe. Aucun champ `alg`, URL de clé ou téléchargement de clé
n'est accepté. JSON ambigu, clés dupliquées, extensions et documents dépassant
16 384 octets sont refusés.

## Appliquer et consulter

- `POST /admin/assessment-reviews/attempts/{attempt_id}/adjudications` reçoit
  l'enveloppe. Il exige Bearer, `learner:read`, `learner:write` et la permission
  `assessment:adjudicate` (owner, admin ou responsable pédagogique). Le compte
  apprenant lui-même, support et service accounts restent exclus. La tentative
  doit déjà avoir une évaluation `host_llm` et être applicable au curriculum.
- `GET .../adjudications/current` renvoie la dernière décision, sa révision et
  l'éventuelle version d'invalidation du curriculum, sous les droits actuels.
- `accept` sélectionne cet avis comme évaluation externe pour les lectures de
  preuve. Les scores sont revérifiés contre la rubrique figée au stockage.
- `reject` retire la confiance courante de cette tentative. Il peut être utilisé
  après purge des textes, puisque leurs hashes restent conservés. Pour accepter
  un autre avis après un désaccord, obtenir un nouveau certificat qui référence
  cet avis et la révision courante.

La première écriture répond `201`, la relance valide `200` avec `replayed: true`.
Une relance expirée est refusée ; utiliser la consultation pour récupérer le
résultat historique. Une révision périmée ou une clé de certificat réutilisée
avec un contenu différent reçoit `409`. Un certificat invalide reçoit `400`.
Les réponses et les logs ne reproduisent pas les erreurs de parseur.

## Effet et historique

L'arbitrage ajoute un journal immuable ; la note du tuteur, l'avis initial,
les interactions, BKT et FSRS restent intacts. Les lectures simples et groupées
sélectionnent la dernière décision **avant** leurs filtres et limites. Un avis
accepté peut donc corriger réussite ou échec dans les preuves de maîtrise et de
transfert. Une révision ultérieure peut retirer cette confiance sans effacer
l'historique ni traiter la réponse comme un nouvel apprentissage.

Le journal conserve l'enveloppe signée et la clé publique utilisée, avec IDs,
empreintes, compte opérateur et date serveur. Il n'y a pas de copie supplémentaire
de la réponse ou de la justification. Les signatures prouvent l'attestation de
l'autorité, pas la véracité sémantique de cette attestation.

SQLite `0067_assessment_adjudications` et PostgreSQL
`postgres_0058_assessment_adjudications` ajoutent contraintes, immutabilité et RLS.
L'audit `assessment.adjudicate` est atomique. Le manifeste DSAR dénombre les lignes et l’effacement inclut
le journal, y compris pour les anciennes demandes reprises. Réappliquer
`deploy/postgres-roles.sql` après migration pour les lectures et purges worker.

Le traitement humain vérifiable des domaines à enjeux élevés, l'intégration
effective d'un prestataire et les mesures de sa qualité restent des travaux
d'exploitation et de validation. Aucun prestataire ni clé n'est activé par ce lot.
