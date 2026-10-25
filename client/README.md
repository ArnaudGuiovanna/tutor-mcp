# Tutor — client de bureau

Application Tauri pour apprendre avec Codex ou Claude via Agent Client Protocol (ACP).
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

L’installateur inclut **Codex ACP 2.2.2**, **Claude ACP 0.95.0**, leurs
runtimes compatibles et **Node.js 24.13.0**. Aucune installation npm/Node séparée
n’est nécessaire pour ces deux agents. Choisissez l’agent, puis connectez votre
compte fournisseur. Le dossier de séance est préparé automatiquement. Une
installation WSL2 existante reste accessible dans les réglages avancés.

La connexion Claude ouvre le parcours officiel du fournisseur dans une fenêtre
séparée : Tutor ne collecte pas les identifiants Claude. Les dépendances système
requises par le fournisseur (notamment Git Bash pour les usages shell de Claude
sur Windows) restent celles de son CLI. Les outils intégrés de fichiers/terminal
de Claude sont désactivés dans les séances pédagogiques ; ses outils MCP restent
soumis aux permissions de Tutor. Le module vocal natif facultatif de Codex est
exclu de ce client textuel. Les exécutables conservés restent inchangés.

Pour reconstruire les ressources depuis le dépôt :

```powershell
python client/scripts/prepare-agents.py
```

Le script utilise les archives npm verrouillées et le ZIP officiel Node vérifié
par SHA256. Les ressources générées sont ignorées par Git. Le workflow teste
les deux agents installés sans Node externe dans PATH.

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

**Institution** : créez une identité locale (ou importez une clé Nostr `nsec`),
enregistrez sa sauvegarde chiffrée sur un support sûr. Une fois le fichier écrit
et relu, la confirmation est automatique et le badge passe à **Sauvegardée**.
Les clés restent identiques après un redémarrage avant sauvegarde ; la clé de
récupération est alors temporairement protégée dans le coffre DPAPI. Si un ancien
client a fermé une identité non confirmée sans conserver cette clé, vérifiez le
fichier déjà exporté ; l’application ne remplace jamais silencieusement vos clés.
Une sauvegarde d’une autre identité est refusée.
Le bouton de connexion à Tutor affiche le prérequis restant lorsqu’il est grisé.
L’identité utilise secp256k1 et les signatures BIP340, comme Buzz. L’application
présente uniquement les clés publiques `npub` de l’identité et de sa récupération.
L’administrateur autorise ces deux clés publiques dans l’institution ; le client
ne crée pas de compte par e-mail et ne demande pas de mot de passe de compte.

Saisissez l’origine HTTPS de l’institution, sans `/mcp`, puis vérifiez son nom et
son origine. Le client découvre `/.well-known/tutor-institution` et signe un défi
NIP-98 unique valable 120 secondes. La clé privée reste dans Rust, protégée pour
l’utilisateur Windows par DPAPI. La variante Unix de développement utilise des
fichiers privés `0600`, sans prétendre les chiffrer sur le poste.

La sauvegarde portable est une enveloppe `tutor-identity-nip49` version 1 avec
**deux clés chiffrées séparément au format standard NIP-49 `ncryptsec`**. Scrypt
(logN 16, 64 Mio), normalisation NFKC et XChaCha20-Poly1305 sont implémentés par
la bibliothèque Nostr. La phrase chiffre la sauvegarde et ne sert pas à se
connecter. Conservez le fichier hors ligne et sa phrase séparément. La clé privée
de récupération ne reste pas dans le coffre actif après confirmation ; son
équivalent public permet à l’institution de vérifier une récupération future.
Avant cette confirmation, aucune session institutionnelle ne peut être ouverte.

Le serveur retourne une session révocable de 30 minutes. Aucun jeton permanent
de rafraîchissement n’est conservé : l’application signe un nouveau défi à
l’approche de l’expiration. Seul le jeton d’accès temporaire de la session MCP est
délégué à l’agent. La clé privée, la sauvegarde et sa phrase ne passent jamais dans
ses paramètres, son environnement ou ses prompts. Une déconnexion révoque la
session distante et retire son accès local ; les erreurs de révocation sont
signalées. Les anciens coffres OAuth sont conservés sans être réutilisés.

Le menu **Mon compte et mes rôles** affiche l’identité locale, l’institution et
les rôles renvoyés par le serveur. Créez ou sélectionnez plusieurs comptes locaux
sur le même client ; chacun possède son coffre, ses conversations et son dossier
de travail. Un changement termine l’agent et révoque l’accès Tutor du compte
précédent. Un compte déjà connecté retrouve l’institution par une nouvelle signature
lors de sa sélection. Le compte historique reste dans son dossier d’origine.

Les activités **Concevoir et publier**, **Accompagner mes groupes**, **Apprendre**
ouvrent des sessions avec les seules permissions utiles. Elles n’accordent aucun
rôle propriétaire ou formateur. Un responsable doit autoriser les clés des
nouveaux comptes dans l’institution. Un membre d’équipe peut explicitement
**Activer mon espace apprenant** : seul son rôle apprenant personnel est ajouté,
ses droits d’équipe sont conservés et aucune inscription n’est créée.

Dans **Mes formations**, utilisez **Créer une formation** pour obtenir un vrai
brouillon, puis **Concevoir avec mon agent** pour construire le programme. Le
bouton **Examiner et publier** retrouve les versions réelles à partir de la
formation, affiche modules, objectifs, critères et blocages. Après examen et
confirmation, **Publier dans mon institution** publie la version et ouvre une
offre dans le catalogue de tous les membres. Chacun s’inscrit volontairement.
Une publication ultérieure remplace l’offre automatique du catalogue ; les
apprenants déjà inscrits conservent leur version et leur progression. Une
version publiée est immuable : préparez une nouvelle version pour la modifier.
Un identifiant d’intention locale ne doit jamais être utilisé comme `version_id`.

La conversation occupe l’espace disponible, avec un texte de 20 px sur écran
standard, titres, listes, emphase, tableaux et liens. Les repères se déplient en
panneau flottant. Navigation clavier, lien d’évitement, focus visible, mouvement
réduit et reflow sont pris en compte ; cela ne constitue pas un audit de
conformité RGAA complet.

La rotation exige l’approbation de la clé de récupération actuelle, lue
temporairement depuis la sauvegarde hors ligne. Une clé active volée ne peut donc
pas remplacer à elle seule la récupération. La rotation crée une nouvelle paire et son fichier de sauvegarde avant de
révoquer l’ancienne paire dans l’institution sélectionnée. La récupération signe
avec l’ancienne clé hors ligne et installe une paire neuve. Gardez les deux
sauvegardes jusqu’à la reconnexion réussie ; une erreur réseau après envoi peut
rendre l’issue distante incertaine. Un échec d’écriture locale n’efface pas
l’ancien coffre. Une récupération confirmée impose une nouvelle connexion.

### Identité depuis le terminal

Le binaire Rust `tutor-client-identity` est utilisable sans interface graphique.
Les secrets sont lus **sur stdin**, jamais dans les arguments ni les sorties.
Utilisez un gestionnaire de secrets ou un terminal privé (la saisie stdin brute
peut être visible dans un terminal). Exemple, phrase fournie par votre outil de
secrets :

```bash
cargo run --manifest-path client/src-tauri/Cargo.toml --no-default-features \
  --bin tutor-client-identity -- identity generate \
  --directory /chemin/prive/coffre --file /support-hors-ligne/identite.json
```

Sous-commandes : `generate` crée la paire et peut exporter/confirmer avec `--file` ;
`exportpub` exige une sauvegarde confirmée ; `backup --file FICHIER_NEUF` termine
une création non sauvegardée ; `import --file FICHIER` restaure une sauvegarde
(phrase sur stdin) ; `import --nsec` importe la clé privée lue sur stdin puis
exige une sauvegarde. Une identité différente ne remplace jamais implicitement
un coffre existant. Chaque commande affiche seulement l’état et les clés publiques.

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

Pour construire l’installateur, préparer les ressources des agents puis générer les notices selon le
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
