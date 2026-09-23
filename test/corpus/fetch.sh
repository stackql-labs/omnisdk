#!/usr/bin/env bash
# Populate the provider-document corpus at a PINNED registry commit.
#
# The corpus is a vendored subset of an external registry and is not tracked by git, so a clean
# clone does not have it. This script is the only supported way to create or refresh it: the commit
# is fixed here, so two machines running it get byte-identical documents and a document changing
# upstream cannot read as a code regression.
#
# Usage:  ./test/corpus/fetch.sh
#         OMNISDK_CORPUS_DIR=/tmp/check ./test/corpus/fetch.sh   # write elsewhere, e.g. to verify
set -euo pipefail

REGISTRY_URL=https://github.com/stackql/stackql-provider-registry.git
REGISTRY_COMMIT=3434e05dfee821baeaf952b9f8cf690d7c2a9a29

# The providers the tests and documented examples address. Deliberately a subset: the full registry
# is far larger and nothing here reads the rest.
PROVIDERS=(anthropic aws azure entra_id googleadmin googleapis.com)

DEST=${OMNISDK_CORPUS_DIR:-test/corpus/registry}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Blobless partial clone: history without file contents, then only providers/src is materialised.
git -c advice.detachedHead=false clone --quiet --no-checkout --filter=blob:none \
  "$REGISTRY_URL" "$tmp/reg"
git -C "$tmp/reg" sparse-checkout set --no-cone providers/src
git -C "$tmp/reg" checkout --quiet "$REGISTRY_COMMIT"

# The checkout is what we assert on, not the ref it came from: a moved branch cannot change what
# this produces, and a wrong commit fails here rather than as a puzzling test failure later.
got=$(git -C "$tmp/reg" rev-parse HEAD)
if [ "$got" != "$REGISTRY_COMMIT" ]; then
  echo "corpus: expected $REGISTRY_COMMIT, got $got" >&2
  exit 1
fi

rm -rf "$DEST"
mkdir -p "$DEST"
for p in "${PROVIDERS[@]}"; do
  src="$tmp/reg/providers/src/$p"
  if [ ! -d "$src" ]; then
    echo "corpus: $p is absent from $REGISTRY_COMMIT" >&2
    exit 1
  fi
  cp -R "$src" "$DEST/$p"
done

echo "corpus: ${#PROVIDERS[@]} providers at ${REGISTRY_COMMIT:0:7} -> $DEST"
