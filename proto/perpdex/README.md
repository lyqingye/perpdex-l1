# PerpDEX proto root

Place every `.proto` file that belongs to a custom PerpDEX module under this
directory, following the layout `proto/perpdex/<module>/<version>/*.proto`.

For example, a hypothetical `x/perp` module would live at:

```
proto/perpdex/perp/v1/perp.proto
proto/perpdex/perp/v1/tx.proto
proto/perpdex/perp/v1/query.proto
proto/perpdex/perp/v1/genesis.proto
```

Run `make proto-gen` (requires Docker) to regenerate the Go bindings after
editing any `.proto` file. The generated Go files end up next to the
existing module code at `x/<module>/types/*.pb.go`.
