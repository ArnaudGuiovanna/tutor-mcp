# Contributing to Tutor MCP

Thanks for your interest in contributing. This project is single-author maintained, but PRs and issues are welcome — small, focused changes are easier to review and land than large refactors.

## Reporting bugs

Before opening a bug:

1. Check that no [existing issue](https://github.com/ArnaudGuiovanna/tutor-mcp/issues) covers it.
2. Reproduce against `main` (the binaries from the latest release are rebuilt from `main` on each refresh).
3. Open a new issue using the **Bug report** template. Include:
   - Steps to reproduce
   - Expected vs actual behavior
   - Environment (Go version, OS, runtime version)
   - Logs at `LOG_LEVEL=debug` if relevant

## Reporting security vulnerabilities

**Do not open a public issue** for security vulnerabilities. See [SECURITY.md](./SECURITY.md) for the private disclosure channel.

## Suggesting features

Open an issue using the **Feature request** template. Describe the user-facing problem and your proposed solution. Discussion happens on the issue before any code is written — please don't open a PR without an issue first, except for small fixes (typos, single-line bugs).

## Pull request workflow

1. **Fork** the repository and create a topic branch from `staging`:
   ```bash
   git checkout -b fix/short-description origin/staging
   ```
2. **Make focused commits**. One logical change per commit. Conventional commit prefixes are encouraged: `fix(scope):`, `feat(scope):`, `docs:`, `test:`, `refactor:`, `perf:`, `chore:`.
3. **Add tests** for any behavior change. The project uses table-driven tests; mirror the pattern of nearby `_test.go` files.
4. **Verify locally** before pushing:
   ```bash
   go build ./...
   go vet ./...
   go test ./...
   ```
   All three must pass. GitHub Actions repeats vet, race-enabled tests, cross-platform builds, a real PostgreSQL DB suite, and a blocking vulnerability scan.
5. **Open the PR against `staging`** (not `main`). Fill in the PR template.
6. **Be responsive to review**. Small revisions usually land within a few days.

## Coding conventions

### Formatting

- `gofmt` mandatory.
- `go vet ./...` must pass with no diagnostics.
- Group imports: stdlib, then third-party, then internal. The Go toolchain handles this.

### Tests

- New behavior requires a regression test that fails on `staging` without the fix and passes with it. PR descriptions should state this explicitly.
- Use the existing in-memory SQLite helpers (`setupToolsTest`, `setupCalibTest`, …) rather than inventing new ones.
- For storage changes, run the DB suite on PostgreSQL as well when available:
  ```bash
  TUTOR_TEST_PG_DSN='postgres://tutor:dev@localhost:5432/tutor_test?sslmode=disable' go test -race -count=1 ./db
  ```
- Avoid mocks where a real in-memory store works.

### Documentation

- Update `CHANGELOG.md` for user-visible changes (new tools, contract changes, breaking fixes).
- Update the README only if behavior or surface visible to operators changes.
- Update `docs/regulation-design/` if you touch the regulation pipeline.

## What's in scope

- Bug fixes against the open issue list (especially p0 and p1)
- Adversarial QA findings (open as `[QA]` issues)
- Documentation improvements
- New MCP tools that fit the existing surface conventions (chat-only, JSON I/O, learner-scoped)
- Performance improvements with a measured baseline

## What's out of scope

- Replacing the supported SQLite single-node MVP profile without an explicit migration and operations design
- Presenting the experimental PostgreSQL/multi-node profile as production-ready before shared memory, crash recovery, and an outbox delivery design exist
- Adding a non-chat surface (iframe, web UI, mobile app)
- Vendoring the LLM (the runtime never embeds a model — the LLM stays the LLM)
- New algorithms without a published reference (BKT/FSRS/IRT/PFA/KST follow specific papers — additions need the same rigor)

## Release process

Releases are alpha/beta tags on the `main` branch. Binaries for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` are attached to each GitHub release. The release body documents fixes since the previous tag.

Tags currently floating during the alpha (e.g. `v0.3.0-alpha.1` is refreshed in place on substantive fixes) — once we hit `v1.0.0`, tags become immutable.

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](./LICENSE).
