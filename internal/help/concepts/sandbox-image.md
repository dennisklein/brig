---
title: "Sandbox image"
summary: "The container image a sandbox runs, kept in the VM's Podman. It is separate from the VM's base image."
aliases: ["container image", "podman"]
see: [template, base-image]
commands:
  - "brig image push dev localhost/pi-agent:latest"
  - "brig agent build dev claude"
---
Build agent images with Podman on this host, then push them into a VM.
brig agent build does both for an agent that brig agent init set up, and
points its template at the new image.
