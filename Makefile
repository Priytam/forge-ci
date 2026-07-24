.PHONY: setup db server runner web build test test-e2e fmt-check demo clean

NODE ?= $(HOME)/.nvm/versions/node/v22.16.0/bin

setup: db
	go mod download
	cd web && PATH="$(NODE):$$PATH" npm install

db:
	docker compose up -d postgres

server:
	go run ./cmd/server

runner:
	go run ./cmd/runner --executor=shell

runner-docker:
	go run ./cmd/runner --executor=docker

web:
	cd web && PATH="$(NODE):$$PATH" npm run dev

build:
	go build -o bin/forge-server ./cmd/server
	go build -o bin/forge-runner ./cmd/runner
	cd web && PATH="$(NODE):$$PATH" npm run build

test:
	go vet ./...
	go test ./...

# End-to-end integration suite (build-tagged so `make test` never runs it).
# Builds the real binaries, provisions a throwaway Postgres DB, starts a server
# + shell runner, and drives pipelines through the HTTP API. Needs Postgres on
# :5433 (make db); docker-gated subtests self-skip when `docker info` fails.
test-e2e:
	go test -tags e2e -v -timeout 600s ./test/e2e/...

# gofmt gate used by CI: fails (listing files) if anything is unformatted.
fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

demo:
	./scripts/trigger-demo.sh

# Full containerized stack: postgres + server + runner + web on :3000
up:
	docker compose --profile full up --build

clean:
	rm -rf bin web/dist
