.PHONY: setup db server runner web build test demo clean

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

demo:
	./scripts/trigger-demo.sh

# Full containerized stack: postgres + server + runner + web on :3000
up:
	docker compose --profile full up --build

clean:
	rm -rf bin web/dist
