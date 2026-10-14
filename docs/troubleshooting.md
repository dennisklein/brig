# Troubleshooting

Start with `brig doctor`, which checks the host and says how to fix what it
finds. For a sandbox, `brig sandbox logs dev N --since 10m --source sandbox`
shows what it tried and why a request was denied; [unblock an
agent](how-to/unblock.md) covers denials.

## Host

### passt cannot write uid_map

**`brig start` or the *VM network* check of `brig doctor` fails, and passt's
log says `Couldn't write to /proc/N/uid_map: Operation not permitted`.** brig
runs passt as root in pasta's user namespace, so passt needs the `setfcap`
capability there to sandbox itself, which the SELinux policy of passt
0^20261002 does not grant it. `sudo ausearch -m AVC -c passt` shows the denial
(`{ setfcap } … tclass=cap_userns`). Until the policy allows it, the
`brig-selinux` package loads a module that does; `dnf install brig` installs
it with SELinux policy. If you installed brig with mise or `go install`, add
the module yourself:

```sh
echo '(allow passt_t self (cap_userns (setfcap)))' > /tmp/brig-passt.cil
sudo semodule -i /tmp/brig-passt.cil
```

`sudo semodule -r brig-passt` removes it again, which you want once
`brig-selinux` is installed.

### Other network failures

`brig doctor`'s *VM network* check names the likely cause of other failures to
start a VM's network, such as a passt that is too old, no systemd user
manager, or user namespaces turned off, and prints passt's and pasta's log.
`sudo ausearch -m AVC -ts recent` shows SELinux denials, which also end pasta
and passt silently.

## VM and gateway

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `brig sync` says a secret is missing | No keyring entry with exactly those attributes | `brig secret list dev` on the host; `brig secret set dev github` stores it again |
| `brig start` says it held back a profile change | The change adds endpoints to a credential already in the gateway | Review with `brig sync dev --dry-run`, then `brig sync dev` |
| `--driver-config-json` is rejected | The VM was last started by an older brig, so its gateway still has driver config off | `brig stop dev && brig start dev` |
| Version warning at `brig start` | Host CLI and gateway differ | Older CLI: `sudo dnf upgrade openshell`. Newer CLI, e.g. after a system update: `brig image build && brig upgrade dev --allow-minor` after reading the release notes, or `sudo dnf downgrade openshell` |
| A host on the LAN or on this host is unreachable, though the policy allows it | The VM's network profile, fence 2, blocks it | `brig show dev`, then `brig update dev --profile NAME` and restart the VM; see [network profiles](reference/network-profiles.md) |

### Files missing after a crash

**Files in `~` are missing after a host crash or disk error.** The VM's boot
repaired its data disk and moved what it could not place. Look in
`/home/agent/lost+found` (`brig ssh dev -- ls -la lost+found`) and in the boot
log (`brig ssh dev --root -- journalctl -b -u 'systemd-fsck@*'`).

## Sandboxes and agents

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `create` cannot find the image | It is only in the host's Podman | `brig image push dev localhost/IMAGE:TAG`, and use the `localhost/` name |
| Sandbox stays in `Provisioning` | Invalid policy or provider; condition `ConfigurationInvalid` | `openshell sandbox get N -o json`, fix, then wait; after 300 s it turns to `Error` and needs `sandbox start` |
| Claude Code says it cannot connect to Anthropic services | `api.anthropic.com` is denied: the provider is not attached, or the profile's `binaries` lacks `/usr/local/bin/claude` | `brig sandbox logs dev N --since 10m --source sandbox`, fix `profiles/claude-code.yaml`, `brig sync dev` |
| Claude Code asks you to log in, or bills an API key | The token expired, or an `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` in the sandbox wins over it | Renew with `claude setup-token` and `brig sync dev`, then start Claude Code from a new `sandbox exec` shell; detach the provider that sets the key |
| Pi gets `policy_denied` from its model endpoint | The profile's `binaries` lacks node's real path (`readlink -f /usr/bin/node` in the sandbox), or its `host` is not the host in `baseUrl` | Fix `profiles/llm.yaml`, `brig sync dev`, restart Pi |
| Any tool denied although a rule exists | Binary path differs, for example `/usr/local/bin` vs `/usr/bin` | `brig sandbox exec dev N -- sh -c 'readlink -f "$(command -v TOOL)"'` and list that path |
| `git push` denied, clone works | Profiles allow fetch only | Add the project's push rule ([GitHub](how-to/git-access.md#grant-push-and-pull-requests-for-one-repository), [GitLab](how-to/git-access.md#grant-push-and-merge-requests-for-one-project)) |
| Every `glab` project call denied | `%2F` in the API path | `allow_encoded_slash: true` on the GitLab endpoint |
| Writes to `~` fail in the sandbox | Only the workspace and `/tmp` are writable | Point config at `/sandbox`: `PI_CODING_AGENT_DIR`, `GH_CONFIG_DIR`, `GIT_CONFIG_GLOBAL` |
| The agent cannot change files in a mounted host directory | [Sandbox mounts](how-to/share-directories.md) are read-only | Clone the repository into `/sandbox` and push a branch |
