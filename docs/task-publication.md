# Publication en fin de tâche

Convention du mainteneur : un lot terminé est commité et poussé sur `staging`,
puis fusionné en fast-forward et poussé sur `main`. Aucun déploiement n'est lancé.
Les contributeurs externes continuent à ouvrir des PR vers `staging`.

## Activation du hook Codex

Le dépôt fournit `.codex/hooks.json` avec un événement `Stop`. Codex doit charger
le projet comme projet de confiance ; ouvrir `/hooks` dans le CLI et examiner
puis approuver cette définition. Une définition modifiée nécessite une nouvelle
revue. Il peut être nécessaire de rouvrir la session pour charger le fichier.
Ne pas désactiver la vérification de confiance pour contourner cette étape.

Le mécanisme et cette revue sont décrits dans la
[documentation officielle des hooks Codex](https://learn.chatgpt.com/docs/hooks).
Le CLI local inspecté est `0.153.4`, avec la fonctionnalité `hooks` activée.
La présence du fichier ne prouve pas son activation dans un autre hôte/API.

`Stop` signifie fin de tour, pas nécessairement fin de tâche. Le hook ne publie
donc que si une intention de publication a été préparée explicitement dans la
même session. Une réponse d'analyse, une demande de précision, un sous-agent,
un tour en mode plan ou un arrêt sans intention ne déclenchent pas de push.
Les consignes de `AGENTS.md` demandent aussi l'exécution explicite avant le
message final, afin que celui-ci puisse rapporter le résultat réel.

## Préparer et publier

Prérequis : Git, Bash, Python 3.9+ sur POSIX, Go compatible avec le projet,
identité Git et accès au remote `origin`. Les permissions ordinaires restent
applicables ; le script ne configure ni identifiants ni dérogation au sandbox.

Avant une nouvelle tâche, avec un arbre suivi propre :

```bash
git fetch origin
git switch staging
git merge --ff-only origin/staging
git merge --ff-only main
git merge --ff-only origin/main
```

Une fois le travail achevé et le diff examiné, fournir une liste de fichiers
explicites, y compris nouveaux fichiers et suppressions. Pas de répertoire,
glob ni ajout global :

```bash
python3 scripts/finish_task.py prepare --message "fix: description du lot" -- fichier.go fichier_test.go
python3 scripts/finish_task.py publish
```

La préparation refuse un index déjà rempli. Elle écrit dans le répertoire Git
une intention contenant le parent, l'arbre indexé, le message, le remote et
`CODEX_THREAD_ID` si disponible. Sans identifiant de session, l'appel explicite
à `publish` fonctionne, mais le hook ne publie pas automatiquement cette intention.

La publication :

1. Vérifie l'absence d'opération Git inachevée et de modification suivie hors index.
2. Recharge les branches distantes ; exige que leur histoire et celle de `main`
   soient déjà contenues dans le parent du lot. Une divergence exige une revue.
3. Exécute `scripts/verify-task.sh` : tests hors ligne du publisher, build, vet et
   suite Go complète. `TUTOR_GO_BIN` permet de choisir le binaire Go local.
4. Vérifie que le parent et l'arbre indexé n'ont pas changé durant les tests,
   puis crée le commit sur `staging` et le pousse.
5. Fusionne ce commit dans `main` avec `--ff-only`, puis le pousse sans force.
6. Vérifie les deux références distantes, conserve un reçu local et revient sur
   `staging`. Un nouvel appel sans intention est sans effet.

Les fichiers non suivis hors lot, comme un fichier de passation personnel, ne
sont pas ajoutés. Les modifications suivies sans rapport bloquent la procédure
au lieu d'être cachées dans un stash. Les tests s'exécutent dans l'environnement
de travail courant ; ils ne constituent pas une construction hermétique ni une
preuve de succès de la CI GitHub, qui s'exécute séparément après les pushes.

## Échecs, annulation et reprise

- Un échec de tests laisse les fichiers indexés et l'intention en place, sans
  nouveau commit. Pour modifier le lot, annuler l'intention puis réexaminer et
  désindexer explicitement les seuls fichiers concernés avant de préparer à nouveau.
- Si le commit existe mais qu'un push échoue, l'intention conserve son SHA.
  Après examen de l'erreur, `publish` reprend ce commit : il ne crée pas un
  nouveau commit et ne réécrit pas les branches.
- Il est possible que `staging` ait été publié et que `main` ne le soit pas
  encore. La procédure est ordonnée, pas une transaction distante entre les
  deux pushes. Elle ne tente pas de rollback destructeur.
- Pour annuler l'automatisation : `python3 scripts/finish_task.py cancel`.
  L'index et les commits restent intacts. Une instruction explicite de ne pas
  publier prévaut toujours sur la convention automatique.
- Un verrou commun empêche deux exécutions de ce publisher de se chevaucher.
  Il ne verrouille pas tous les autres clients Git ; les vérifications et les
  pushes sans force détectent les changements concurrents pertinents.
- Le hook demande au plus une continuation en cas d'échec ; il ne boucle pas
  indéfiniment et ne contourne pas une protection de branche ou une autorisation.

Les fichiers de reprise sont `tutor-task-finish.json` et `tutor-task-last.json`
dans le répertoire Git local, jamais dans les fichiers publiés du projet.

Tests du workflow :

```bash
python3 -B -m unittest discover -s scripts -p test_finish_task.py -v
```

Tous les pushes de ces tests ciblent des dépôts bare temporaires locaux, pas
GitHub. Ils couvrent les rejets, les notes préservées, la concurrence et la
reprise après une interruption de commit ou de push.
