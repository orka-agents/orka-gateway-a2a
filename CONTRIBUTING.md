# Contributing to Orka Gateway A2A

This project welcomes contributions and suggestions. Most contributions require you to agree to a
Contributor License Agreement (CLA) declaring that you have the right to, and actually do, grant us
the rights to use your contribution. For details, visit [Contributor License Agreements](https://cla.opensource.microsoft.com).

External contributors are required to sign the Microsoft CLA. If a CLA bot is enabled on the
repository, it will determine whether you need to provide a CLA and decorate the PR appropriately
(for example, status check or comment). Follow its instructions. If no bot responds, ask a maintainer
how to complete the CLA check before merging; the requirement still applies. You will only need to
sign once across all repos using this CLA.

## Code of Conduct

Help us keep this project open and inclusive. Please read and follow our [Code of Conduct](CODE_OF_CONDUCT.md).

## Security issues

Please do not report security vulnerabilities through public GitHub issues. See [SECURITY.md](SECURITY.md)
for security reporting guidance.

## Issues and feature requests

Use [GitHub Issues](https://github.com/orka-agents/orka-gateway-a2a/issues) to report bugs and request
features. Before opening a new issue, please search existing issues to avoid duplicates. For feature
work, please open or comment on an issue before starting a large implementation so maintainers can
confirm the direction.

Keep the adapter focused on translating its documented A2A subset to Orka's existing gateway APIs.
Execution, retries, retention, and cleanup belong in Orka, not a second adapter-owned task store or
scheduler. Changes to native AI dispatch or the gateway contract belong in the upstream Orka project.

## Commit sign-off

Please sign commits with a Developer Certificate of Origin style `Signed-off-by` line:

```bash
git commit -s -m "type(scope): describe the change"
```

## Pull requests

Before submitting a pull request:

1. Fork the repository and create a branch for your change.
2. Keep the change focused and avoid unrelated edits.
3. Add or update tests and documentation when they are relevant to the change.
4. Run the appropriate build, lint, and test commands.
5. Open a pull request with a clear description of the change and verification performed.

## Build and test

Use Go 1.25 or newer for the module. CI tests Go 1.25.x and Orka's current Go 1.27.x;
use Go 1.27 for linting to match CI. Tests do not need a cluster or a model service.

```bash
make build              # Build bin/orka-gateway-a2a and bin/a2a-client
make test               # Run all Go tests without cached results
make test-race          # Run tests with the race detector (requires a C compiler)
make vet                # Run go vet
make fmt                # Format Go code
make lint               # Lint all packages, not only changed lines
make lint-fix           # Run lint with automatic fixes; review the diff afterward
make docker-build       # Build orka-gateway-a2a:local; does not push
```

`make lint` and `make lint-fix` run **golangci-lint v2.13.1** through `go run` by
default. This downloads the tool into Go's cache without changing the module's
dependencies. You can install the same version separately and use that binary:

```bash
# Run with Go 1.27 to match CI.
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1
"$(go env GOPATH)/bin/golangci-lint" version
make lint GOLANGCI_LINT="$(go env GOPATH)/bin/golangci-lint"
```

Check that `version` reports **2.13.1**. To change the local container tag, use
`make docker-build IMG=orka-gateway-a2a:my-change`.

Before submitting, also run `go mod verify`. CI checks formatting, module
integrity, tests, race tests, vet, both binaries, full-package lint, and a container
build. It does not deploy, push images, or call cloud models. The full lint rules
are in [.golangci.yml](.golangci.yml).

See [Getting started](docs/getting-started.md) to run the adapter and
[Compatibility](docs/compatibility.md#verification-and-limits) for what the tests do
and do not prove.
