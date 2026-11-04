# Tutor ACP 0.0.12 · Tutor-MCP v0.7.4

Cette release réunit le client Windows et le serveur qui préparent le contexte
pédagogique avant la génération par Codex ou Claude. Elle devient la release
**Latest** pour l'installateur et les archives serveur.

## Client ACP 0.0.12

- Contexte regroupé : formation sélectionnée, notions utiles, difficultés,
  observations récentes, séance active et décision du serveur pour une activité.
- Sessions ACP réutilisées et réglages du modèle conservés. Le harness gère
  l'authentification et la facturation ; aucun choix clé/API ou abonnement
  supplémentaire n'est imposé par le client.
- Préparation des quiz, soumission des réponses et enregistrement des évaluations
  effectués par le client natif, avec validation du serveur avant affichage.
- Reprise d'un enregistrement interrompu avec la même clé, sans nouvelle
  génération ni duplication de l'observation pédagogique.
- Une réponse déjà enregistrée reste terminée si Stop arrive juste après.
- Consolidation mémoire en arrière-plan, avec priorité à la conversation,
  lorsque le stockage narratif partagé et les autorisations sont disponibles.

Le workspace, les thèmes, les fichiers texte joints, le partage et les titres
de conversation des versions précédentes sont conservés. L'agent local reste
présent avec son périmètre expérimental antérieur ; il n'utilise pas le nouveau
parcours préparé destiné aux harness Codex et Claude.

## Installation et serveur

Télécharger **`Tutor_ACP_0.0.12_windows_x64-setup.exe`** et l'installer par-dessus
la version précédente. L'identifiant de l'application reste `fr.aguiovanna.tutor` ;
les profils, les conversations et les réglages restent dans leur emplacement
habituel. Les adaptateurs Codex et Claude et leurs runtimes sont inclus.

Le serveur **v0.7.4** ajoute `prepare_pedagogical_turn`, réduit les schémas des
outils et conserve l'autorité sur les décisions, les tentatives et la progression.
La consolidation utilise des lots bornés, des baux et des contrôles de version.
Les migrations SQLite et PostgreSQL sont additives.

Les six archives serveur et leurs notices sont jointes. Mettre aussi à jour
le serveur MCP pour activer le contexte regroupé ; un serveur plus ancien
conserve le parcours historique du client. Publier cette release ne déploie
pas automatiquement le serveur d'une institution.

## Vérifications et limites

Les 19 contrôles GitHub requis doivent réussir sur le commit publié. Le workflow
Windows construit, installe et démarre l'application, vérifie les adaptateurs,
le parcours WSL2, le clavier, les thèmes et la persistance des réglages.
Les rapports joints identifient le commit, les workflows et les fichiers livrés.
`SHA256SUMS` couvre tous les assets à l'exception de lui-même.

La préparation du contexte a été mesurée localement sur données synthétiques.
La cible de **une à cinq secondes pour une réponse complète** n'est pas encore
qualifiée avec les modèles réels sur Windows et WSL. Aucun abaissement silencieux
du modèle ou de son niveau de réflexion n'est appliqué.
