#!/usr/bin/env sh

# Generate OpenAPI v2 (Swagger) definitions from every Query/Service .proto
# file under ./proto. The merged result is emitted into ./tmp-swagger-gen
# and is intended to be consumed by `swagger-combine` if you maintain a
# Swagger UI on top of the chain.
#
# Uses POSIX-only sh constructs so it runs under BusyBox `ash` inside the
# proto-builder Docker image.

set -eu

mkdir -p ./tmp-swagger-gen
cd proto

query_files=$(find . -type f \( -name 'query.proto' -o -name 'service.proto' \))
if [ -z "$query_files" ]; then
  echo "No query.proto/service.proto files found under ./proto, nothing to generate."
  exit 0
fi

for file in $query_files; do
  buf generate --template buf.gen.swagger.yaml "$file"
done
