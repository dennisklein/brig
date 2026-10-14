# Git access

A GitHub or GitLab provider gives an agent read access: clone, fetch and API
reads. Pushes and pull or merge requests stay denied until a policy grants
them for a named repository, so grant writes per project, not in the shared
default.

## GitHub with gh

The [agent images](agents.md) install `git` and `gh` at `/usr/bin`, the
paths in OpenShell's
[`providers/github.yaml`](https://github.com/NVIDIA/OpenShell/blob/v0.1.4/providers/github.yaml),
and make `gh` git's credential helper, so git sends the placeholder in Basic
auth for the proxy to resolve. Copy that profile into `profiles/` unchanged.
It sends `GITHUB_TOKEN` or `GH_TOKEN` as a bearer token to `api.github.com`
(REST and GraphQL, read-only) and `github.com` (clone and fetch only).

Create a fine-grained token limited to the repositories agents may touch, with
Contents and Pull requests read-write and Metadata read, and reference it from
`providers/github.yaml` as in [config
directories](../concepts/openshell-config.md#config-directories). With
`--provider github`, `gh` works without `gh auth login`.

## Grant push and pull requests for one repository

Keep a policy file per project next to the default, here
`policies/webapp.yaml`: the default's content plus these rules. Pass it at
create with `--policy`, or apply it to a running sandbox with `openshell
policy set webapp --policy policies/webapp.yaml --wait`.

```yaml
network_policies:
  github_push_webapp:
    name: github-push-webapp
    endpoints:
      - host: github.com
        port: 443
        protocol: rest
        enforcement: enforce
        rules:
          - allow: { method: GET,  path: "/alice/webapp.git/info/refs*" }
          - allow: { method: POST, path: "/alice/webapp.git/git-upload-pack" }
          - allow: { method: POST, path: "/alice/webapp.git/git-receive-pack" }
    binaries:
      - path: /usr/bin/git
  github_prs_webapp:
    name: github-prs-webapp
    endpoints:
      - host: api.github.com
        port: 443
        protocol: rest
        enforcement: enforce
        rules:
          - allow: { method: POST, path: "/repos/alice/webapp/pulls" }
          - allow: { method: POST, path: "/repos/alice/webapp/issues/*/comments" }
    binaries:
      - path: /usr/bin/gh
```

The agent opens pull requests and comments on them, but cannot merge, close
them or change the repository. `gh pr create` uses GraphQL, which the github
profile keeps read-only in every sandbox, so have the agent use REST:
`gh api repos/alice/webapp/pulls -f title=TITLE -f head=BRANCH -f base=main`.
`git-receive-pack` lets it push any branch, `main` included: protect `main`
on GitHub with a ruleset that requires a pull request, so that only your
review merges. Always set `enforcement: enforce` on your own endpoints: on an
inspected endpoint, `audit` is the default and only logs.

## GitLab with glab

OpenShell ships no GitLab profile, so write one. One host serves the API and
git: `glab` sends the token as `PRIVATE-TOKEN`, `git` as a Basic-auth
password. Replace `gitlab.example.org` with your instance, or `gitlab.com`.
Like GitHub's, the profile allows reads and clones but not pushes:

```yaml
# profiles/gitlab-work.yaml
id: gitlab-work
display_name: GitLab (work)
description: GitLab API and git over HTTPS on gitlab.example.org
category: source_control
credentials:
  - name: api_token
    description: GitLab personal or project access token
    env_vars: [GITLAB_TOKEN]
    required: true
    auth_style: header
    header_name: private-token
discovery:
  credentials: [api_token]
endpoints:
  - host: gitlab.example.org
    port: 443
    protocol: rest
    enforcement: enforce
    allow_encoded_slash: true        # API paths look like /projects/group%2Fproject
    rules:
      - allow: { method: GET, path: "**" }
      - allow: { method: HEAD, path: "**" }
      - allow: { method: OPTIONS, path: "**" }
      - allow: { method: POST, path: "/**/git-upload-pack" }
binaries: [/usr/bin/glab, /usr/local/bin/glab, /usr/bin/git, /usr/local/bin/git]
```

Without `allow_encoded_slash`, OpenShell's proxy rejects the `%2F` in GitLab's
project paths, so every `glab` call on a project is denied.

The profile lets sandboxes read anything on the host that the token can, so
use a project or group access token with the Developer role, limited to the
projects agents work on: `api` scope, or `read_api` plus `read_repository`
while the agent only reads. A personal token would also open every other
project you can see, and the CI/CD variables of those you maintain.

```sh
secret-tool store --label='GitLab token (agents)' service gitlab.example.org user alice
```

```yaml
# providers/gitlab-work.yaml
name: gitlab-work
type: gitlab-work
credentials:
  GITLAB_TOKEN:
    secret_tool:
      lookup: [service, gitlab.example.org, user, alice]
```

In the image, install `glab` at `/usr/bin/glab`, point it at your host, and
give `git` a credential helper that reads the same variable. Inside a
checkout, `glab` also finds the host from the git remote.

```dockerfile
ENV GITLAB_HOST=gitlab.example.org
RUN git config --system credential.https://gitlab.example.org.helper \
      '!f() { test "$1" = get && printf "username=oauth2\npassword=%s\n" "$GITLAB_TOKEN"; }; f'
```

## Grant push and merge requests for one project

Add to that project's policy file:

```yaml
network_policies:
  gitlab_push_tools:
    name: gitlab-push-tools
    endpoints:
      - host: gitlab.example.org
        port: 443
        protocol: rest
        enforcement: enforce
        allow_encoded_slash: true
        rules:
          - allow: { method: GET,  path: "/group/tools.git/info/refs*" }
          - allow: { method: POST, path: "/group/tools.git/git-upload-pack" }
          - allow: { method: POST, path: "/group/tools.git/git-receive-pack" }
          - allow: { method: POST, path: "/api/v4/projects/group%2Ftools/merge_requests" }
          - allow: { method: POST, path: "/api/v4/projects/group%2Ftools/merge_requests/*/notes" }
    binaries:
      - path: /usr/bin/git
      - path: /usr/bin/glab
```

The last two rules let `glab mr create` and `glab mr note` open merge requests
and comment on them, but not merge them or change the project. Protect the
default branch so that the agent's pushes cannot reach it. Some `glab` commands look a project up by numeric ID or use
GraphQL at `/api/graphql`; if one is denied, take the exact path from
`openshell logs SANDBOX --source sandbox` and add it. Keep `enforcement:
enforce` here: an `audit` rule would overlap the provider's `enforce`
endpoint, and OpenShell rejects that.
