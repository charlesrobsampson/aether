#!/usr/bin/env bash
# Thin wrapper around internal/docs::TestDocs, which validates the README, the
# operator guides, relative Markdown links, the CI workflow, and the absence of
# a committed go.work.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

go test -v ./internal/docs -run '^TestDocs$'
