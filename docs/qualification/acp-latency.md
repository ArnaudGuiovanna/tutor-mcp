# Qualification de la latence ACP

L'objectif est une réponse complète en au plus 5 secondes. Il n'est pas acquis.
Un fragment, une animation, un délai d'abandon ou une réponse non enregistrée
ne constituent pas un succès. Les tests logiciels et les initialisations ACP en
CI ne mesurent pas le temps d'une réponse réelle de Codex ou Claude.

## Mesure native avec un vrai modèle

Depuis Windows, utiliser `client/tests/windows/qualify-latency.ps1`. Le runner
crée un dossier neuf, un profil client et une base Tutor jetables. Il ne remplace
pas les réglages du client habituel et laisse l'authentification et la facturation
au harness. `-LiveModel` autorise explicitement les appels au fournisseur.

```powershell
.\client\tests\windows\qualify-latency.ps1 `
  -LiveModel -Harness codex -Environment native -Scenario resume `
  -Adapter 'C:\outils\codex-acp.cmd' -TutorBinary 'C:\outils\tutor-mcp.exe' `
  -TestDirectory 'C:\mesures\codex-native-reprise-01' `
  -Model '<identifiant annoncé par le harness>' -Effort high -Speed balanced -Turns 3
```

Pour WSL, lancer la commande depuis Windows avec `-Environment wsl`,
`-Distribution Ubuntu`, `-AgentDirectory /home/utilisateur/mesures-nouvelles` et
des chemins **Linux** pour `-Adapter` et `-TutorBinary`. Le répertoire Linux doit
être neuf et son parent exister ; `-TestDirectory` reste un chemin Windows neuf.
`-WslUser` est facultatif. Pour Claude, utiliser `-Harness claude` et son adaptateur.
Choisir uniquement des identifiants de modèle et d'effort effectivement annoncés
par cet adaptateur. Un réglage non pris en charge provoque un échec explicite.

Le runner réapplique les paramètres **après** la connexion : le changement de
cible remet auparavant les préférences enregistrées à zéro. Il vérifie les
`currentValue` ACP avant le premier prompt et après chaque réponse, et les compare
au contrat demandé. Les intentions sauvegardées ne suffisent pas à qualifier une
configuration. Modèle absent, configuration ambiguë ou changement de réglage
invalident la mesure. Si modèle/effort sont omis, le reçu conserve les valeurs
annoncées ; il ne les assimile pas à un réglage explicitement demandé. Le mode
rapide peut correspondre au drapeau du fournisseur ou à une préférence d'effort :
`speedMechanism` distingue ces mécanismes.

| Scénario | Parcours mesuré | Prérequis et limites |
| --- | --- | --- |
| `resume` | Reprise structurée puis message « Reprendre la formation … », alternativement | Vraie formation d'algèbre créée atomiquement par `init_domain`, avec critère généré par Tutor. Première réponse à froid, suivantes à chaud. Le modèle ne doit appeler aucun outil. |
| `explanation` | Explication courte de conservation de l'égalité | Préparation native et réponse complète. Première réponse à froid, suivantes à chaud. Aucun appel outil du modèle. |
| `correction` | Soumission d'une réponse « Je ne sais pas… » à un exercice ouvert réellement généré et figé, puis correction enregistrée | Formation diagnostique fraîche par échantillon, sans forcer le routage après un échec. Génération de l'énoncé mesurée séparément dans `prerequisites`. Corrections **à chaud uniquement** ; `coldMeasured:false`. Une introduction ou une sortie non figée ne peut servir de faux exercice. |
| `context-read` | Lecture explicite de `get_learner_context` par le modèle | Sonde historique du coût d'un aller-retour MCP, distincte de la reprise optimisée. |

« À froid » signifie ici le premier prompt d'apprentissage après connexion du
processus et ouverture de sa session. `connectMs` est séparé ; le démarrage complet
de l'application n'est pas inclus. Le fournisseur peut conserver ses propres
caches. Une correction à froid après fermeture/réouverture doit être mesurée
dans l'application sur un exercice déjà enregistré.

Le reçu privé `latency.json` version 2 contient la provenance compilée, les
configurations effectives, les phases, les appels ACP, le temps jusqu'à la fin
du traitement natif et la confirmation d'une nouvelle réponse persistée. Il
exclut texte d'apprenant, arguments d'outils, réponses de modèle et identifiants
de connexion distants. Le profil jetable contient nécessairement les messages
du scénario ; ne pas le publier comme un simple fichier de métriques.

Un statut `passed` exige tous les tours complets sous 5 000 ms, une configuration
vérifiée, une seule génération par tour, la réutilisation de session attendue et
des diagnostics complets. Pour les nouveaux parcours, il exige aussi une sortie
native persistée, le bon contexte de formation lorsqu'il y en a un et l'absence
d'outil appelé par le modèle. Une erreur, une annulation, un quota, un refus,
un mauvais format ou un dépassement restent `incomplete` avec la phase d'arrêt.
Le reçu conserve toujours `uiRenderMeasured:false` et `pedagogyQualified:false`.

## Mesure dans la fenêtre Windows

Effectuer les parcours dans le client construit à la même révision, sur une
formation de test isolée, avec le vrai serveur et le vrai harness. Relever les
versions exactes client/serveur/adaptateur, modèle, effort, rapidité, environnement,
connexion, et l'heure. Vérifier la configuration réellement annoncée dans le
client ; ne pas déduire le modèle du seul nom d'un fichier de résultats.

Le diagnostic local `window.tutorResponseTimings.export()` renvoie un document
JSON borné aux 100 dernières mesures en mémoire. Il n'envoie aucune donnée. Le
copier depuis les outils de développement de la fenêtre de qualification après
les parcours ; `window.tutorResponseTimings.clear()` remet le collecteur à zéro.
Ne pas confondre cet export avec celui d'une conversation.

Chaque événement `tutor:response-rendered` contient :

```json
{
  "version": 1,
  "conversationId": "identifiant local",
  "assistantMessageId": "identifiant local",
  "operationId": "présent seulement si reçu du backend pour ce prompt",
  "elapsedMs": 4200,
  "recordedAt": "horodatage ISO du marqueur",
  "uiRenderMeasured": true,
  "measurement": "after-two-animation-frames"
}
```

Cet exemple décrit le format, **pas un résultat de mesure**. Le chronomètre part
de l'envoi/reprise dans l'interface. Le marqueur n'est émis qu'après retour du
backend, lecture de la réponse complète et persistée, validation du contenu et
présence dans le DOM après deux trames d'animation. Une réponse privée, interrompue
ou en attente d'enregistrement n'émet pas de succès. Les trames d'animation sont
un marqueur DOM ; elles ne prouvent pas le moment exact du paint à l'écran.

Corréler les opérations d'un **même** essai avec leurs traces natives et serveur.
Ne pas joindre un reçu Rust et un export UI issus de deux générations différentes
pour fabriquer une mesure de bout en bout. Un parcours déclenché qui ne produit
aucun marqueur compte comme essai incomplet, pas comme une observation à supprimer.

## Matrice de réception et preuves encore nécessaires

| Environnement | Harness | Reprise bouton et texte | Explication | Correction |
| --- | --- | --- | --- | --- |
| Windows natif | Codex | À froid / à chaud | À froid / à chaud | Exercice déjà enregistré après redémarrage / à chaud |
| Windows + WSL | Codex | À froid / à chaud | À froid / à chaud | Même protocole |
| Windows natif | Claude | À froid / à chaud | À froid / à chaud | Même protocole |
| Windows + WSL | Claude | À froid / à chaud | À froid / à chaud | Même protocole |

À chaque essai, contrôler réponse finale, formation sélectionnée, décision
serveur, tentative figée avant réponse, absence de double enregistrement et
absence de progression créée par une simple reprise ou explication. Refaire un
cas d'ambiguïté entre deux formations et un changement de formation. Vérifier
les reprises après erreur de réseau et commit différé séparément des succès.

Cette matrice est un protocole, pas une déclaration de validation. Les résultats
Windows et Claude réels doivent être joints avant toute revendication de latence
sur ces configurations. Une mesure Linux ou un faux serveur ne les remplace pas.
Rapporter tous les essais, leur maximum et leurs échecs, avec les mêmes paramètres
et tailles de réponse ; un percentile ou une moyenne ne prouve pas un plafond
absolu de 5 secondes. Les temps de génération externes restent variables.

## Essais réels Linux du 5 novembre 2026

Essais sur le candidat non publié, avec le serveur candidat `dev`, des bases
jetables et les comptes existants des harnesses. Les mesures de reprise ont été
recueillies avant la montée de version 0.0.13 (le runner annonce encore 0.0.12) ;
le dernier contrôle de correction Claude utilise le candidat 0.0.13. Aucun appel au serveur de
production. Adaptateurs épinglés : Codex ACP 2.2.2 et Claude ACP 0.95.0. Les
paramètres ont été vérifiés dans les réponses ACP avant et après les prompts.
Le reçu embarque les empreintes des sources compilées ; ce sont des essais de
candidat, pas une qualification d'un installateur publié ni de GitHub CI.

Les valeurs ci-dessous vont de l'envoi natif à la réponse complète enregistrée.
La connexion initiale au harness et le rendu de la fenêtre sont séparés. Chaque
paire comprend un premier prompt d'apprentissage et un prompt suivant dans la
même session, avec des réponses générées différentes.

| Harness et configuration annoncée | Reprise avant, froid / chaud | Reprise après, froid / chaud | `session/load` froid avant → après |
| --- | --- | --- | --- |
| Codex, `gpt-6.1-sol`, `high`, fast-mode `off` | 19,801 s / 15,583 s | 19,535 s / 11,861 s | 1 (359 ms) → 0 |
| Claude, alias `sonnet`, `xhigh`, rapidité par défaut | 22,810 s / 9,767 s | 14,871 s / 6,999 s | 1 (6 038 ms) → 0 |

Le changement vérifié concerne la première session : elle est créée directement
sans outils pour une reprise native reconnue, au lieu d'être créée avec MCP puis
rechargée immédiatement sans outils. Après correction : une création, aucun
rechargement, un seul `session/prompt` à froid ; aucune création ni rechargement
et un seul prompt à chaud. Aucun appel outil du modèle dans ces parcours.

| Après correction du rechargement | Temps du RPC `session/prompt`, froid / chaud | Création de session froide |
| --- | --- | --- |
| Codex `high` | 19,072 s / 11,746 s | 220 ms |
| Claude `sonnet` / `xhigh` | 11,931 s / 6,868 s | 2 589 ms |

`session/prompt` englobe génération, traitement du harness et transport ; ces
traces ne permettent pas de les répartir plus finement. La disparition du reload
est établie par le journal RPC. La variation du temps de génération entre essais
ne peut pas être attribuée à cette modification.

Autres scénarios exécutés avec de vraies réponses :

| Scénario | Configuration | Réponses complètes |
| --- | --- | --- |
| Explication courte, froid / chaud | Codex `gpt-6.1-sol` / `high` / balanced | 22,098 s / 5,499 s |
| Correction, deux échantillons chauds | Même configuration Codex | 7,720 s / 6,720 s |
| Reprise diagnostique avec réflexion faible, avant retrait du reload | Codex `gpt-6.1-sol` / `low` / balanced | 12,994 s / 10,775 s |
| Explication courte, froid / chaud | Claude, alias `sonnet` / `xhigh` / défaut | 13,768 s / 7,940 s |
| Correction après précision du contrat numérique, deux échantillons chauds | Même configuration Claude | 6,741 s / 5,248 s |

Les préparations réelles des deux exercices Codex ont pris respectivement
24,202 s et 11,364 s, et celles des deux exercices Claude 17,109 s et 6,737 s ;
elles précèdent la soumission de la réponse et ne sont pas incluses dans la durée
de correction. Le profil `low` est une comparaison
diagnostique isolée : aucun réglage utilisateur n'a été modifié. Le modèle
canonique derrière l'alias Claude `sonnet` n'est pas attesté par ces seuls reçus.

Des essais préalables incomplets restent conservés : le premier a été refusé
avant génération par une liste d'autorisations du runner incomplète, ensuite
corrigée ; une première série de corrections a produit une correction de
6,984 s puis une introduction choisie par le serveur après l'échec. Le runner
n'a pas transformé cette introduction en faux exercice. La série suivante
utilise une formation diagnostique fraîche pour chaque échantillon.

Une correction Claude a également été refusée après 6,706 s de RPC modèle :
son score contenait `confidence:"medium"`, alors que le validateur attend un
nombre entre 0 et 1. Le contrat de génération précise désormais ces types et
bornes. Aucun score textuel n'est converti arbitrairement ; la tentative refusée
est restée soumise, sans interaction évaluée ni progrès partiel. Le nouveau test
Claude a ensuite enregistré deux corrections complètes, avec un score numérique
valide ; ces deux durées restent supérieures à cinq secondes.

Contrôle des bases jetables, sans exporter leur contenu : les reprises Codex et
Claude ont créé deux tentatives préparées chacune, **zéro interaction** et aucun
événement d'apprentissage. Les deux corrections Codex ont produit exactement
deux interactions, dans deux domaines, et deux tentatives évaluées, toutes deux
échouées et non qualifiées de confiance indépendante. Deux événements `response`
sont présents ; le dernier parcours Claude présente les mêmes comptages. Aucun
feedback n'est déclaré affiché par ce runner sans UI.

**Le plafond de cinq secondes échoue sur tous ces échantillons complets.** La
suppression des appels outils du modèle et du reload initial ne suffit pas à
l'atteindre avec les configurations mesurées. Windows natif, WSL, le rendu réel
dans la fenêtre, les corrections à froid et un volume représentatif d'essais
restent à qualifier. Aucune durée Linux ne vaut validation Windows.
