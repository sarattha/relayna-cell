SHELL := /bin/bash
VM := relayna-cell
.PHONY: test build build-linux vm-up install observability-up smoke inference-up agents-build model-smoke dashboards-up dashboards-check
test:
	go test -race ./...
	go vet ./...
build:
	mkdir -p bin
	go build -o bin/cell ./cmd/cell
build-linux:
	mkdir -p bin/linux-arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o bin/linux-arm64/cell ./cmd/cell
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o bin/linux-arm64/cell-init ./cmd/cell-init
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o bin/linux-arm64/cell-server ./cmd/cell-server
vm-up:
	limactl start --tty=false --name=$(VM) deploy/lima/cell.yaml
install: build build-linux
	bash scripts/install.sh
observability-up:
	limactl shell $(VM) sudo docker compose -f /opt/relayna-cell/deploy/observability/compose.yaml up -d
smoke:
	bash scripts/smoke.sh
inference-up:
	bash scripts/inference-up.sh
agents-build:
	bash scripts/agents-build.sh
model-smoke:
	bash scripts/model-smoke.sh
dashboards-up:
	bash scripts/dashboards-up.sh
dashboards-check:
	python3 scripts/build-dashboards.py --check
	python3 scripts/check-dashboards.py
