---
title: "Run tests that need Docker in a sandbox"
concepts: [sandboxes, policies]
commands:
  - "brig sandbox create dev --docker -- --name webapp --from localhost/webapp-ci:2026-10-20"
doc: "how-to/docker.md"
---
1. Create the sandbox with --docker. brig adds the rule brig_docker to its
   policy and sets DOCKER_HOST and testcontainers' settings.
2. Run the tests. testcontainers needs no docker CLI; docker and docker
   compose need it in the image.
3. Publish fixed ports from 61000 to 61127, on 127.0.0.1; the rule allows
   only those.

Containers that the sandbox starts run as the VM's user dockerd, outside the
sandbox's policy but inside the VM's network profile.
