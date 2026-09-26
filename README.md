# iazio-harness

`iazio-harness` runs one IDE CLI job for a single checkout and streams the child's output.

`iazio-agent` starts this command. It uses the same public client pattern as `iazio-harvester` and `iazio-mcp`. Product surfaces that schedule work live in `iazio-web` and `iazio-api`.

## Test

```bash
go test ./...
```

## Build

```bash
go build -o iazio-harness ./cmd/iazio-harness
```

Link-time variables are `main.version`, `main.commit`, and `main.branch`.
When `main.commit` is set, `iazio-harness version` prints `iazio-harness <version>+<shortsha>`.

## Commands

```text
iazio-harness version
iazio-harness auth status
iazio-harness auth login
iazio-harness run --worktree-path PATH --docs-hub-path PATH --job-id ID --api-url URL
```

`auth status` prints whether a refresh token is stored. It does not call the network and it does not print the token. OAuth endpoints come from the environment (`IAZIO_AUTH_URL`, `IAZIO_TOKEN_URL`, `IAZIO_DEVICE_AUTH_URL`) or from `~/.iazio/harness.json`. `IAZIO_HARNESS_CONFIG` overrides that path.

The public client id is `iazio-harness-cli`.
