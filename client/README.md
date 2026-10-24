# Tutor — client de bureau

Application Tauri pour apprendre avec Codex via Agent Client Protocol (ACP).
Le client est développé dans ce dossier du dépôt Tutor ; le serveur pédagogique
reste à la racine. La cible de référence est **Windows x64**. Un agent peut être
exécuté nativement ou dans une distribution **WSL2** choisie explicitement.

## Parcours disponibles

- Connexions séparées à Codex et à Tutor, avec état réel et erreurs explicites.
- Détection de l’adaptateur et de Codex sur Windows et dans WSL2 ; chemin manuel.
- Intention personnelle révisable, validation humaine et conservation locale.
- Conception et tutorat avec les vrais outils MCP, formations et catalogue du
  serveur, conversation progressive, permissions explicites et interruption.
- Conservation des échanges sur l’appareil et reprise de la séance agent avec
  le même compte Tutor. Les réponses interrompues restent identifiées comme telles.

Une intention locale devient un parcours pédagogique quand l’agent l’enregistre
effectivement dans Tutor. Les inscriptions, les permissions et les observations
restent contrôlées par le serveur. Les estimations ne constituent pas une preuve
de maîtrise. Le client ne crée ni votes ni publication communautaire inexistants.

## Installation Windows et Codex

Le workflow **Client → Client (Windows)** construit un installateur NSIS et
conserve l’artefact `tutor-client-windows-x64-<commit>` pendant 14 jours. Il teste
l’installation et la création d’une fenêtre native ; une compilation locale
Linux ne vaut pas qualification Windows. Les installateurs de développement
ne sont pas signés avec un certificat commercial.

**Codex Desktop n’est pas requis.** Le processus lancé est l’adaptateur
`@agentclientprotocol/codex-acp`, qui embarque une dépendance Codex compatible.
Une installation Codex détectée est informative : la sélection automatique
utilise ce runtime fourni. En configuration manuelle, remplir **Chemin de Codex**
choisit explicitement ce CLI via `CODEX_PATH`, après vérification de son accès et
de `--version`. Sous Windows, sélectionner le binaire natif `codex.exe`, et non
un wrapper npm `.cmd`. Sous WSL, utiliser un chemin Linux absolu vers le CLI
exécutable. Laisser ce champ vide conserve le runtime fourni par l’adaptateur.
La négociation et les opérations réelles de l’agent vérifient ensuite la
compatibilité du couple choisi.

L’adaptateur de référence est **2.2.2**, avec ses dépendances exactes conservées
dans `scripts/adapter/package-lock.json`. Pour le préparer dans ce dépôt :

```powershell
npm ci --prefix client/scripts/adapter --ignore-scripts
```

Dans les réglages avancés, sélectionner ensuite le chemin absolu de
`client\scripts\adapter\node_modules\.bin\codex-acp.cmd`. Le client résout ce
wrapper vers Node et son script, sans interpréter une commande par `cmd.exe`.
Cette distribution npm nécessite Node.js. Une installation globale de la même
version ou un exécutable autonome compatible peut également être sélectionné.
L’installateur Tutor ne télécharge pas silencieusement un agent ou un modèle.

Pour WSL2, installer l’adaptateur **dans la distribution Linux concernée**, puis
relancer la détection. Le lancement conserve le même utilisateur Linux, retrouve
son environnement Codex, et passe par `wsl.exe`. Bash et `setsid` (`util-linux`)
sont nécessaires. Les chemins `~/...`, Linux absolus et Windows sont résolus dans
la distribution avant la création de la session ACP. Le nettoyage termine le
groupe de processus de cette séance, sans arrêter la distribution.

La connexion au fournisseur IA suit les méthodes annoncées par l’adaptateur.
Le compte ou la clé fournisseur reste dans l’environnement Codex sélectionné.
Les environnements Windows et WSL ne partagent pas automatiquement leurs accès.
La connexion fournisseur et son éventuel coût restent distincts du service Tutor.

## Connexion à Tutor

**Local** : indiquer le binaire Tutor et les arguments `--local`. Le binaire doit
être utilisable dans le même environnement que l’agent : chemin Linux pour WSL,
chemin Windows pour un agent natif. Pour choisir un profil, ajouter
`--data-dir CHEMIN_ABSOLU`. Plusieurs processus Tutor partagent ce profil durable.

**Distant** : saisir l’origine HTTPS de l’institution, sans suffixe `/mcp`.
Le client ouvre le navigateur pour OAuth avec PKCE S256 et un retour loopback.
Il utilise le compte et les invitations actuellement disponibles sur le serveur.
L’identité native par clé décrite dans la vision institutionnelle constitue un
contrat serveur distinct et n’est pas simulée par cette application.

Le serveur doit fournir [l’API de lecture du client](../clientapi/README.md).
Les écrans lisent `/api/client/v1/overview` ; l’agent utilise `/mcp` avec les accès
apprenant. Une institution qui restreint l’enregistrement OAuth peut fournir un
jeton initial DCR ou un identifiant de client public préenregistré. Dans ce dernier
cas, enregistrer exactement `http://127.0.0.1:47823/oauth/callback`.

Les jetons Tutor ne passent pas dans l’interface. Le jeton de rafraîchissement
reste dans la couche Rust et le coffre local ; seul le jeton d’accès apprenant
est délégué à l’agent pour ses appels MCP. Sous Windows, DPAPI chiffre le coffre
pour l’utilisateur Windows courant. La variante Unix sert au
développement et protège son fichier par des droits `0600`, sans prétendre le
chiffrer. Le rafraîchissement renouvelle les accès sans élargir leurs permissions.
La déconnexion retire les accès locaux et ferme l’agent. Le serveur actuel
n’annonce pas d’endpoint de révocation OAuth ; le client le signale et ne prétend
pas annuler à distance un jeton déjà émis.

Les intentions et conversations sont enregistrées dans `workspace.json`, dans
le dossier de données utilisateur Tauri (`fr.aguiovanna.tutor`). Elles peuvent
contenir des informations personnelles. Le fichier du coffre est séparé. Une
corruption est signalée et conservée pour récupération ; elle n’est pas effacée
par une réinitialisation automatique.

Pour sauvegarder l’espace local, fermer Tutor puis copier ce dossier dans un
emplacement privé. Le coffre DPAPI reste lié à l’utilisateur Windows qui l’a
créé : copier le dossier sur un autre compte ne transfère pas automatiquement
les accès. Les données pédagogiques du serveur suivent ses propres procédures
de sauvegarde et de restauration.

## Développement

Préparer Node.js 24, Rust 1.93 et les
[prérequis Tauri pour Windows](https://v2.tauri.app/start/prerequisites/), dont
les outils C++ MSVC et WebView2. Depuis ce dossier :

```powershell
npm ci
npm run tauri -- dev
```

Pour construire l’installateur, générer les notices selon le
[guide de distribution](docs/distribution-notices.md),
puis lancer `npm run tauri -- build --bundles nsis`.

Les commandes natives Rust portent la connexion, les processus et la persistance.
L’interface React/TypeScript appelle uniquement le bridge Tauri. Le navigateur de
prévisualisation affiche l’interface mais ne lance pas un agent ni un faux service.

Depuis la racine du dépôt, la vérification commune est :

```bash
bash scripts/verify-client.sh --install --real-adapter
```

Elle inclut les tests d’interaction, le build web, Rustfmt, Clippy, les tests du
cœur natif et une négociation avec l’adaptateur réel sans appel de modèle. Les
fixtures de protocole restent dans `tests/` et ne sont pas empaquetées avec l’UI.
La [qualification du runtime](docs/runtime-qualification.md) distingue les essais
natifs, WSL et les appels réels de modèle ; les tests synthétiques ne qualifient
pas à eux seuls un tutorat réel.

Les profils `local`, `hobby` et `institution` du serveur restent disponibles.
Un changement de connexion impose une reprise explicite ; une séance d’un compte
ne peut pas être rejouée dans un autre compte ou une autre institution.
