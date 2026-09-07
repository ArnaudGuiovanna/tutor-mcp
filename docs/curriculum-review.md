# Revue sémantique du curriculum

Le canal `/admin/curriculum-reviews/` présente un curriculum figé et conserve
un avis authentifié par compte et version. Il expose la couverture de l'avis,
sans certifier sa justesse ni modifier le graphe, les estimations ou les preuves.

## Parcours

1. `GET /admin/curriculum-reviews/domains/{domain_id}/versions/{version}` renvoie
   `snapshot`, `current_version`, `material_hash` et un ETag fort. Une version
   historique demeure consultable sous les droits actuels.
2. `POST .../versions/{version}/opinions` reçoit directement une liste de
   constats, avec `Idempotency-Key` et `If-Match: "material_hash"` obligatoires.
   Seule la version courante d'un domaine actif peut recevoir un nouvel avis.
3. `GET .../versions/{version}/opinions/mine` restitue l'avis du compte, y
   compris après publication d'une version suivante ou purge de son texte.

```json
[
  {
    "concept_id": "identifiant_stable",
    "aspect": "definition",
    "judgment": "needs_revision",
    "rationale": "Préciser la performance observable attendue."
  }
]
```

Les aspects sont `definition`, `outcomes`, `criteria` et `prerequisite`. Ce
dernier exige `related_concept_id`, également issu du snapshot. Une arête absente
peut être proposée avec `needs_revision` ; elle n'est pas ajoutée au graphe.
Les jugements sont `adequate`, `needs_revision` et `not_assessed`, avec une
justification non vide. Les doublons, IDs étrangers et extensions inconnues
sont refusés. Les limites sont 16 384 octets par document et 2 000 octets par
justification. Un curriculum trop grand pour un avis complet conserve un avis
partiel ; il n'est pas artificiellement marqué comme entièrement examiné.

La couverture attendue comprend trois sections par compétence active et une
section par arête de prérequis existante. Les propositions d'arêtes nouvelles
n'augmentent pas la couverture. La disposition est calculée :

- `changes_requested` si au moins une révision est demandée ;
- `complete_opinion` si toutes les sections sont examinées sans révision demandée ;
- `partial_opinion` sinon.

`certified` reste toujours `false`. Même une couverture complète indique un
avis de compte ; elle n'atteste pas un examen humain effectif ni une validation
expérimentale. La propriété `review` du snapshot original reste intacte.

## Droits et conservation

Les routes exigent Bearer et `learner:read`, puis aussi `learner:write` pour
l'enregistrement. La permission `curriculum:review` est distincte : owner,
admin et responsable pédagogique accèdent à leur organisation ; un formateur
est limité à ses cohortes. Le membership et son état/MFA/version sont revérifiés
en transaction. Apprenants, auditeurs, billing, service et support ne disposent
pas de ce droit par leur rôle seul. L'auto-revue du curriculum personnel est
exclue par `user_id` ; deux comptes d'une même personne ne sont pas détectés.

Un seul premier avis par version et compte est conservé. Une relance identique
restitue l'avis ; une clé réutilisée pour d'autres constats reçoit `409`.
La comparaison porte sur les constats canoniques, triés par section. La révision
du domaine et l'avis se sérialisent par le verrou du domaine, avec audit atomique
`curriculum.review.record`. Les réponses sont `no-store`.

SQLite `0069_curriculum_review_opinions` et PostgreSQL
`postgres_0060_curriculum_review_opinions` ajoutent le journal, ses contraintes
et la RLS. `AssessmentPlaintextDays` purge irréversiblement `findings_json`, en
respectant les gels de conservation. Empreintes, disposition et couverture
restent consultables. Le manifeste DSAR dénombre ces avis et leur effacement
est pris en charge ; les demandes
anciennes reçoivent leur checkpoint manquant. Réappliquer les droits worker.

## Traiter les constats

L'expert propose les changements, puis le propriétaire publie une révision
explicite avec les opérations existantes (`update_metadata`, `repair_prerequisites`,
scission, fusion ou retrait). Le moteur réconcilie les définitions selon sa
politique conservatrice ; l'avis ne peut pas affirmer une équivalence pour
conserver artificiellement une maîtrise.

Pour des estimations antérieures à la réconciliation de septembre, une revue
du curriculum ne répare pas rétroactivement les observations. Documenter les
compétences concernées et prévoir une nouvelle évaluation indépendante. Le coût
des invalidations et la pertinence réelle des prérequis exigent encore des
observations du domaine et des apprenants.
