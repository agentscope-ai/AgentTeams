# Contributing to HiClaw

Thanks for contributing to HiClaw.

## Before You Start

- Read the project overview in [`README.md`](README.md)
- Read local development details in [`docs/development.md`](docs/development.md)
- Check architecture in [`docs/architecture.md`](docs/architecture.md)

## Development Prerequisites

- Docker
- Git
- `mc` (MinIO Client) for integration tests
- `jq` for test scripts

## Local Workflow

1. Create a branch from `main`
2. Make focused changes
3. Build what you changed
4. Run relevant tests
5. Open a pull request with clear context

Common commands:

```bash
make build
make test
make test-quick
make help
```

## Testing Expectations

- For image/runtime changes, run integration tests (`make test` or targeted `TEST_FILTER`)
- For docs-only changes, verify links and formatting
- Do not remove or weaken existing tests to make changes pass

## Changelog Policy

If your change affects built image content under:

- `manager/`
- `worker/`
- `copaw/`
- `openclaw-base/`

add an entry to [`changelog/current.md`](changelog/current.md) before merging.

## Pull Request Checklist

- [ ] Scope is clear and limited
- [ ] Related docs are updated
- [ ] Relevant tests were run
- [ ] No secrets were added
- [ ] Changelog entry added when required
