#!/usr/bin/env bash
set -euo pipefail

task_repo=$(git rev-parse --show-toplevel)
cd "$task_repo"
task_go_bin=${TUTOR_GO_BIN:-go}
# The Snap launcher can hang on profile generation in the development VM.
if [[ -z ${TUTOR_GO_BIN:-} && -x /snap/go/current/bin/go ]]; then
    task_go_bin=/snap/go/current/bin/go
fi
task_git_dir=$(git rev-parse --absolute-git-dir)
task_tmpdir=$(mktemp -d "$task_git_dir/task-validation.XXXXXX")
cleanup_task_tmp() {
    rmdir "$task_tmpdir" 2>/dev/null || printf 'Temporary build files retained for inspection: %s\n' "$task_tmpdir" >&2
}
trap cleanup_task_tmp EXIT
export TMPDIR="$task_tmpdir"
export PYTHONDONTWRITEBYTECODE=1

git diff --cached --check
python3 -m unittest discover -s scripts -p 'test_finish_task.py'
"$task_go_bin" build ./...
"$task_go_bin" vet ./...
"$task_go_bin" test -p 1 ./... -count=1
