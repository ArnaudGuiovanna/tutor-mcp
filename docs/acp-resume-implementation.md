# Reprise native des formations et qualification ACP

Plan d'implémentation — 5 novembre 2026. Reprise autorisée par le mainteneur,
avec agents répartis par fichiers. La note locale du 4 novembre conserve
l'état historique ; sa pause est levée pour ce chantier.

## Résultat attendu

Le client prépare la sélection et la décision pédagogique avant le harness.
Le serveur conserve l'autorité sur la progression, le curriculum, la séance,
les preuves et la notation. Le modèle formule une introduction ou une activité
conforme à cette décision en un seul passage. La reprise couvre le bouton et
les demandes ordinaires de reprise en conversation, sans imposer un QCM.

L'affichage complet, à froid comme à chaud, est la mesure de latence. La cible
souhaitée reste une à cinq secondes ; elle n'est pas prouvée par la suppression
des appels intermédiaires. Toute mesure manquante ou dépassement reste visible.
Un timeout, une animation d'attente ou un premier fragment n'est pas une réponse
complète. Les performances d'un fournisseur externe ne constituent pas une
garantie de délai universelle.

## Lots et responsabilités

1. **État initial et protocole (agent principal).** Actualiser staging en
   fast-forward, préserver les notes locales et le correctif des critères
   `2f16ea6`. Définir les contrats de reprise, d'activité ouverte et de choix
   explicite. Conserver un seul lot de publication final clairement préparé.
2. **Préparation serveur (agent serveur).** Étendre le paquet pédagogique pour
   confirmer la formation, fournir les informations nécessaires sans relecture
   `get_training`, figer la décision canonique et décrire la présentation attendue.
   Résoudre les noms seulement sur les formations autorisées ; gérer les
   homonymes et listes incomplètes sans sélectionner arbitrairement. Préserver
   transactions, versions, autorisations et idempotence.
3. **Orchestration native (agent principal).** Reconnaître les commandes de
   reprise non ambiguës et le contrat explicite, préparer avant génération,
   gérer les choix sans passage modèle inutile. Valider la sortie contre la
   décision ; figer les exercices avant affichage et enregistrer une réponse
   explicite avant feedback. L'aide et le chat ordinaire ne sont pas des réponses.
4. **Interface (agent interface).** Émettre une reprise structurée au clic,
   conserver la demande hors ligne, proposer les choix ambigus et rendre une
   introduction ou un exercice ouvert. Garder quiz/flashcards existants et les
   accusés d'affichage corrects. Vérifier clavier, i18n, partage et réouverture.
5. **Qualification et plateformes (agent qualification).** Vérifier que le
   modèle, l'effort et la vitesse demandés sont réellement appliqués. Exercer
   reprise, explication et correction avec Codex/Claude, Windows natif/WSL ;
   corréler préparation, harness, finalisation et affichage. Déclarer une
   politique cohérente : client Windows, serveur Linux et Windows local ;
   aligner CI, installateurs et artefacts distribués. Les nouvelles releases
   standard ne doivent pas annoncer des artefacts Darwin non qualifiés.
6. **Intégration, livraison et déploiement (agent principal).** Revoir les diffs,
   exécuter les contrôles appropriés, SQLite/PostgreSQL compris, puis publier
   les seuls fichiers du lot par le publisher. Synchroniser explicitement les
   contrôles obligatoires et la politique distante si nécessaire. Après CI,
   publier le nouvel installateur et déployer le serveur sauvegardé. Vérifier
   les versions réellement exécutées et distribuées.

## Contrats et invariants

- `tutor-resume-request` v1 : identité de formation connue au clic, ou résolution
  serveur d'une commande de reprise. Aucune transformation d'une demande longue
  ou ambiguë en sélection silencieuse.
- `tutor-training-choice` v1 : choix autorisés, bornés et explicitement rendus ;
  une clarification ne crée ni séance d'évaluation ni progression.
- `tutor-activity` ajoute `kind=exercise`, avec consigne et rubrique figées,
  tentative confirmée, contexte/version et décision serveur.
- `tutor-response` ajoute une réponse ouverte explicite et bornée. La saisie
  générale de conversation et une demande d'indice ne sont jamais notées par
  simple proximité avec un exercice.
- Une introduction est une instruction, jamais une réussite. Son accusé de
  présentation arrive après rendu effectif ; le recto d'une flashcard ne prouve
  pas l'affichage de son verso.
- Reprises, annulations et rejeux ne dupliquent ni observation ni progression.
  Le changement de formation, de révision, de compte ou d'institution invalide
  les actions devenues incompatibles.
- Les règles de confiance, l'assistance et l'autorité du serveur restent
  inchangées. Aucune baisse de latence ne justifie une preuve inventée.

## Matrice de réception

| Exigence | Preuve requise |
|---|---|
| Reprendre au clic ou par commande ordinaire | Parcours réel : sélection et décision préparées avant le prompt, aucun aller-retour modèle de planification |
| Homonymes ou formation absente | Choix/clarification autorisés, aucune mutation pédagogique indue |
| Introduction / exercice / quiz / flashcard | Format conforme au moteur, pas de conversion systématique en QCM |
| Réponse d'exercice et demande d'aide | Tentative figée, soumission explicite, aide distincte, enregistrement avant feedback |
| Annulation, rejeu et redémarrage | Pas de double consommation, données et outbox cohérentes |
| Isolation et versions | Tests négatifs comptes/institutions/formations/révisions |
| Modèle et configuration | Configuration effective vérifiée ; mesure rejetée si elle diffère |
| Réponse complète en 1–5 s | Mesures réelles à froid/chaud jusqu'au rendu, dépassements conservés, aucune extrapolation d'un test étroit |
| Windows natif et WSL, Codex et Claude | Résultats propres à chaque environnement/harness ; absent = non qualifié |
| Plateformes publiées | Cohérence politique, matrice CI, contrôles obligatoires et archives de release |
| Livraison | Commit exact sur staging/main, CI observée, installateur publié et serveur effectivement mis à jour |

Ce plan ne constitue pas une attestation d'achèvement. Les résultats exécutés,
écarts et blocages doivent être consignés avant toute déclaration de fin.

## État de l'intégration avant publication

Les lots serveur, natif et interface sont reliés au runtime. Les contrôles
ciblés SQLite et PostgreSQL 17, y compris les reprises concurrentes avec le
détecteur de courses, passent. Les parcours natifs simulés vérifient les
introductions, exercices ouverts, demandes d'aide distinctes, enregistrements
repris sans nouvelle génération, annulations et changements de contexte.
La suite complète du client, les adaptateurs ACP réels et les contrôles de
distribution ont également été exécutés ; le publisher doit les reprendre
sur l'arbre final avant de créer le commit et d'attendre GitHub CI.

Les essais réels ont confirmé un seul prompt sans outil modèle par reprise,
puis permis de supprimer le rechargement de la session nouvellement créée.
Une correction Claude a révélé un contrat insuffisamment explicite sur le type
numérique de confiance ; le prompt a été clarifié, sans transformer une étiquette
textuelle en preuve chiffrée. La tentative refusée est restée non évaluée.
Les [résultats et limites](qualification/acp-latency.md) conservent les essais
incomplets et tous les dépassements. Le plafond de cinq secondes reste échoué.

La suite attendue est la publication du commit exact, l'installateur 0.0.13,
la release serveur v0.7.5 et son déploiement sauvegardé. Les mesures jusqu'au
rendu sur Windows natif/WSL nécessitent un hôte Windows avec les vrais harnesses.
La liste des contrôles distants doit être alignée explicitement sur la politique
Windows/Linux avant promotion ; une modification locale de la liste ne suffit pas.
