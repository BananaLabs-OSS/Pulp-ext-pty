# Pulp-ext-pty

`Pulp-ext-pty` provides Pulp's cross-platform pseudo-terminal spawning
capability for applications that need interactive host processes.

## Placement-scoped authority

Hosts using Pulp placement grants assign `spawn.pty` to one exact application
cell placement. The supported grant uses resource `interactive-terminal`,
right `open`, an absolute `root`, an `executables_json` shell-to-path map, a
fixed `environment_json` object, and bounded `max_argc`/`max_arg_bytes`.

A missing or mismatched grant is denied whenever a resolver is configured.
Hosts without a resolver retain the historical behavior. PTY session handles
are always cell-scoped for write, resize, status, and close operations.

## Development

```sh
go test ./...
```

## License

MIT
