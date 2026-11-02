.PHONY: check test frontend build security release-test release-dry-run test-sandbox

check:
	bash scripts/check.sh

frontend:
	npm --prefix web ci
	npm --prefix web run typecheck
	npm --prefix web test
	npm --prefix web run build

test: frontend
	python3 -B -m unittest discover -s scripts -p '*_test.py'
	go test -race -count=1 -timeout=5m ./...

build: frontend
	go build -mod=readonly -trimpath -o bin/new-empire-control ./cmd/control

security:
	go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...
	npm --prefix web audit --omit=dev --audit-level=low

release-test:
	python3 -B -m unittest discover -s scripts -p '*_test.py'
	go test -race -count=1 ./scripts/release

# This creates no tag/release and never reads a private signing key.
release-dry-run: frontend
	python3 -B scripts/release.py plan
	go run ./scripts/release dry-run -version 0.0.0-dev.1 -commit "$$(git rev-parse HEAD)" -out dist/dry-run

test-sandbox:
	bash scripts/test-sandbox.sh
