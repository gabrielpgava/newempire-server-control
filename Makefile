.PHONY: check test frontend build security dist release-plan release-test release-dry-run test-sandbox

OUT ?= dist/release-$(VERSION)

check:
	bash scripts/check.sh

frontend:
	npm --prefix web ci
	npm --prefix web run typecheck
	npm --prefix web test
	npm --prefix web run build

test: frontend
	python3 -B -m unittest discover -s scripts -p '*_test.py'
	python3 -B -m unittest discover -s deploy -p 'test_*.py'
	go test -race -count=1 -timeout=5m ./...

build: frontend
	go build -mod=readonly -trimpath -o bin/new-empire-control ./cmd/control

security:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
	npm --prefix web audit --omit=dev --audit-level=low

# Unsigned local candidates only; production signing stays in the release environment.
dist:
	@test -n "$(VERSION)" || { echo 'VERSION is required (for example: make dist VERSION=0.0.0-dev.1)' >&2; exit 1; }
	python3 -B scripts/release.py check-output --output "$(OUT)"
	$(MAKE) frontend
	go run ./scripts/release build -version "$(VERSION)" -commit "$$(git rev-parse HEAD)" -out "$(OUT)"
	python3 -B scripts/release.py sbom --version "$(VERSION)" --commit "$$(git rev-parse HEAD)" --binary "$(OUT)/new-empire-control_$(VERSION)_linux_amd64.tar.gz" --binary "$(OUT)/new-empire-control_$(VERSION)_linux_arm64.tar.gz" --output "$(OUT)/sbom.cdx.json"

release-plan:
	python3 -B scripts/release.py plan

release-test:
	python3 -B -m unittest discover -s scripts -p '*_test.py'
	go test -race -count=1 ./scripts/release

# This creates no tag/release and never reads a private signing key.
release-dry-run: frontend
	python3 -B scripts/release.py plan
	go run ./scripts/release dry-run -version 0.0.0-dev.1 -commit "$$(git rev-parse HEAD)" -out dist/dry-run

test-sandbox:
	bash scripts/test-sandbox.sh
