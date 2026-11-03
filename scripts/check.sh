#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
python3 -B scripts/check.py repository
unformatted="$(git ls-files -z '*.go' | xargs -0 gofmt -l)"
if [[ -n "$unformatted" ]]; then
  echo "Run gofmt on these files:" >&2
  echo "$unformatted" >&2
  exit 1
fi
go mod verify
make frontend
# Pin the validator too; no floating npx dependency in release validation.
npm exec --yes --package=@redocly/cli@2.72.1 -- redocly lint "$(python3 -B scripts/check.py openapi-file)" --extends minimal
python3 -B -m unittest discover -s scripts -p '*_test.py'
python3 -B -m unittest discover -s deploy -p 'test_*.py'
go vet ./...
go test -race -count=1 -timeout=5m ./...
go build -mod=readonly -trimpath -o bin/new-empire-control ./cmd/control
git diff --exit-code -- go.mod go.sum web/package-lock.json
