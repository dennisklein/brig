# Built-in service types

Each `NAME.yaml` here is the manifest of the service type `NAME`; brig embeds
them at build time. `service.Type` documents the fields. A type whose tokens
are not the default hashed tokens, or that needs work after each start,
also registers a `service.Driver` in `internal/cli/servicedriver.go`.

Manifests are not loaded from the user's configuration yet: the format may
change while the first types shape it.
