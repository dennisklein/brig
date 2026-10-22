---
title: "llm-proxy"
summary: "A service that runs LiteLLM with the model keys, and gives each attached sandbox a LiteLLM key of its own with a budget."
aliases: ["litellm", "virtual key", "model proxy", "budget"]
see: [services, placeholder, provider]
commands:
  - "brig service create models --type llm-proxy --openshell-config ~/brig/models"
  - "brig agent init claude --vm dev"
doc: "how-to/llm-proxy.md"
---
The model keys are providers on the service VM's own gateway, so LiteLLM sees
only their placeholders, and once the service exists brig sync keeps them off
the gateways. A sandbox's key is named brig/GATEWAY/WORKSPACE/SANDBOX in
LiteLLM's UI, at /ui/ on the service's port, where the master key logs in.
