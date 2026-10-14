# Secrets

A provider file in a [config
directory](../concepts/openshell-config.md#config-directories) only says where
a credential is in your keyring; `brig sync` looks it up and hands it to the
gateway. This page shows how to store, check and rotate those secrets, use
another keyring client, and run without a keyring in CI. [Where secrets
go](../concepts/architecture.md#where-secrets-go) explains what happens to
them afterwards.

## Store a secret

With the provider files in place, `brig secret set` stores a secret under the
attributes its file gives, so they cannot differ from what `brig sync` looks
up. It reads the secret without echo or from standard input, and then syncs
just that provider if the VM runs (`--no-sync` skips that):

```sh
brig secret set dev github              # prompts; prints only how long the secret was
claude setup-token | brig secret set dev claude-code
brig secret list dev                    # stored or missing for each credential, never values
```

`secret-tool` does the same by hand, with the attributes from the provider
file:

```sh
secret-tool store --label='GitHub token (agents)' service github.com user alice
secret-tool store --label='LLM key (agents)' service llm.example.com
brig sync dev
```

## Rotate a token

`brig secret set dev github` stores the new token under the same attributes
and updates that provider. With `secret-tool store`, run `brig sync dev`
yourself; brig notices the change either way. Running agents keep the old
token: start them again from a new shell, `brig sandbox exec dev webapp --tty
-- bash -l`, or stop and start the sandbox. `brig help rotate-token` has the
short version.

## Use another keyring client

To use another keyring client, set `openshell.secret_tool` in
`~/.config/brig/config.yaml`; config directories cannot set it, so a shared
one cannot run programs on your host. The program takes two verbs, as
`secret-tool` does: `lookup ATTRIBUTE VALUE...` prints the secret, and `store
--label=LABEL ATTRIBUTE VALUE...` reads it from standard input. A wrapper
around `pass`:

```sh
#!/bin/sh
# ~/.local/bin/pass-secret-tool
verb=$1; shift
case $verb in
lookup) pass show "agents/$(printf %s "$*" | tr ' /' '__')" ;;
store)
	shift # --label=LABEL
	pass insert --multiline --force "agents/$(printf %s "$*" | tr ' /' '__')" ;;
*) exit 2 ;;
esac
```

A wrapper without `store` still works for syncing; `brig secret set` then
prints the attributes so you can store the secret yourself.

## Secrets in CI

A CI job has no keyring. There a credential can name an environment variable
of brig's own instead of a keyring entry:

```yaml
credentials:
  GH_TOKEN:
    env: BRIG_SECRET_GH_TOKEN     # secret_tool or env, not both
```

The name must start with `BRIG_SECRET_`, so a shared directory cannot make
brig read an unrelated variable, such as `AWS_SECRET_ACCESS_KEY`, and send it
to a profile's endpoints. An unset or empty variable fails the sync of that
provider like a missing keyring entry; a changed value is picked up like any
other rotated secret.
