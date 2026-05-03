#!/usr/bin/env sh

# Generate Go bindings for every .proto file under ./proto.
#
# Intended to be invoked through the `make proto-gen` target which mounts
# this repository inside the cosmos proto-builder Docker image. It can also
# be run on the host if `buf` and the `gocosmos` / `grpc-gateway` plugins
# are installed locally.
#
# The proto-builder image runs scripts via BusyBox `ash`, so this file uses
# POSIX-only constructs (no arrays, no process substitution).
#
# Behaviour notes:
# - When no .proto files exist yet (e.g. brand new project) the script
#   succeeds with a no-op message instead of erroring out, so that
#   `make proto-gen` is safe to run as part of bootstrap pipelines.

set -eu

echo "Generating gogo proto code"
cd proto

proto_files=$(find . -type f -name '*.proto')
if [ -z "$proto_files" ]; then
  echo "No .proto files found under ./proto, nothing to generate."
  exit 0
fi

# Group files by directory and feed each directory's protos to buf one at
# a time. We rely on $IFS-based word splitting which is fine for our
# project layout (no spaces in proto paths).
proto_dirs=$(echo "$proto_files" | xargs -n1 dirname | sort -u)
for dir in $proto_dirs; do
  for file in $(find "${dir}" -maxdepth 1 -name '*.proto'); do
    if grep -q "option go_package" "$file"; then
      buf generate --template buf.gen.gogo.yaml "$file"
    fi
  done
done

cd ..

# `buf generate` writes generated files under the import path inside the
# repo root (e.g. `github.com/perpdex/perpdex-l1/...`). Move them next to
# the source they belong to and clean up the leftover directory tree.
if [ -d "github.com/perpdex/perpdex-l1" ]; then
  cp -r github.com/perpdex/perpdex-l1/* ./
  rm -rf github.com
fi
