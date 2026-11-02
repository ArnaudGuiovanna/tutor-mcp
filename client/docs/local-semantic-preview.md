# Préversion Windows 0.0.9 : intentions et consultations Tutor

Cette préversion conserve Qwen3-1.7B Q4_K_M et le moteur CPU embarqué. Elle permet
de tester un premier routage des formulations libres vers le profil et la
bibliothèque du serveur Tutor, sans modifier ce serveur. Elle ne qualifie pas
encore le coaching complet ni les performances de la Quadro P2000.

## Fonctionnement livré

Le client active `--prefix-cache on --reuse-mcp on --catalog-cache on
--semantic-router on`. Les commandes fixes existantes restent disponibles.
Pour une demande à classer, le modèle reçoit une petite taxonomie, le message
actuel et le statut vérifié de la formation active. Il ne reçoit ni catalogue
d’outils, ni schémas MCP, ni identifiants protégés. Une génération JSON d’au plus
64 tokens propose une catégorie et, éventuellement, des mots de recherche.

Rust vérifie cette proposition, choisit le contrat dans le catalogue réel,
construit les arguments, demande la permission ACP et journalise l’opération.
Le profil et les résultats de recherche sont affichés directement depuis les
reçus vérifiés. Ils n’exigent pas une seconde génération pour leur rédaction.
Un refus de permission empêche l’appel. Aucun échec de transport n’entraîne de
rejeu automatique. La bibliothèque affiche au plus dix éléments et signale une
page partielle sans promettre une continuation déjà fonctionnelle.

Les interdictions de consultation et les formulations pédagogiques simples
peuvent contourner le classificateur. Ces règles sont bornées : elles ne
constituent pas une compréhension générale de la négation. La conversation
conserve le projecteur existant : 2 048 tokens, 192 réservés à la réponse, 256
de marge, au plus deux tours récents et le contexte obligatoire vérifié.
Le journal complet reste distinct du prompt. Le classement utilise seulement
la demande actuelle ; les références comme « la deuxième » et les workflows
de sélection, création, activité et évaluation restent à intégrer.

## Essai sur Windows

Installer la préversion, connecter son serveur Tutor et choisir l’agent local.
Le modèle déjà téléchargé reste utilisable. Pour chaque essai, relever le temps
avant le premier affichage et le temps total, en distinguant le premier tour
du suivant et le temps passé à répondre à une permission.

1. « Donne moii mon profil » puis « Tu sais quoi sur moi ? » : vérifier les
   informations contre celles du serveur et le comportement après refus.
2. « Quels cours je pourrais suivre ? » puis « Je voudrais voir les formations
   accessibles » : vérifier la liste et l’absence de formations inventées.
3. « Quels cours de Python je pourrais suivre ? » : vérifier le filtre de recherche.
4. « Sans consulter mon profil ni mes formations, explique ce qu’est un objectif
   d’apprentissage » : aucune consultation ne doit être proposée.
5. Une question pédagogique simple : vérifier la pertinence de la réponse libre,
   séparément de la réussite du routage.

Le backend empaqueté utilise **le CPU**, avec `gpu-layers=0`. Cette release ne
prouve ni une accélération P2000 ni une latence acceptable sur la machine cible.
Le premier chargement du GGUF et une classification préalable peuvent encore
retarder le premier texte. Le serveur MCP conserve sa connexion habituelle.

## Banc reproductible de développement

`client/scripts/qualify-local-semantic.py` exécute huit demandes connues sur un
serveur local jetable et un vrai GGUF, avec permissions, reçus et journal SQLite.
Le contrat de rendu a été figé avant le premier essai. Les rapports successifs
conservent les échecs ; ce corpus a servi au développement et n’est pas un test
indépendant de généralisation. Les tests Python synthétiques vérifient le banc,
pas la compréhension du modèle.

```sh
python3 -B client/scripts/qualify-local-semantic.py --execute \
  --repo . --agent /chemin/tutor-local-agent --server /chemin/tutor \
  --model /chemin/Qwen3-1.7B-Q4_K_M.gguf \
  --contract docs/local-harness/semantic-render-contract.json \
  --output client/scripts/semantic-machine-test.json --timeout 1800 --turn-timeout 180
```

Le fichier de sortie doit être nouveau et se trouver dans `client/scripts/`.
Le banc enregistre les empreintes des sources, exécutables, poids et contrat,
les appels observés, les refus et les temps. Il ne mesure pas la qualité
pédagogique, l’interface ni les percentiles de latence.

Le [troisième rapport Linux CPU](../../docs/local-harness/semantic-development-r3.json)
termine les huit scénarios, avec les consultations et rendus attendus. Il conserve
**130 contrôles réussis sur 131** : aucune preuve `relay_end` n’a été observée,
donc la fermeture de tous les descendants MCP reste non prouvée. L’agent lui-même
s’est arrêté proprement. Ce rapport reste marqué `failed_or_incomplete`.
Les deux premiers rapports en échec sont conservés ; les propositions du modèle
et les règles de routage ont été améliorées entre ces essais de développement.

Observations uniques du troisième essai, sur Linux CPU à quatre threads :
la première classification de profil affiche son résultat en 12,04 s ; la
paraphrase suivante en 1,95 s ; les deux demandes de formations en 1,92 s et
2,25 s. Le premier texte de la réponse libre initiale arrive en 24,39 s,
chargement inclus. Ces mesures ne prédisent pas celles de Windows et ne
constituent ni des percentiles ni une comparaison contrôlée de performances.
