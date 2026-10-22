// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package llmproxy is the llm-proxy service type: LiteLLM and its database
// in an OpenShell sandbox of the service VM, whose providers hold the model
// keys, and a LiteLLM virtual key for each attached sandbox. It holds the
// image that brig builds for the sandbox, the check of the service's
// llm-proxy.yaml, and the service.Driver.
package llmproxy
