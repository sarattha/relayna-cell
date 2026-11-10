#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
GOBIN="$PWD/bin/tools" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10
PATH="$PWD/bin/tools:$PATH" protoc -I internal/observer/proto \
  --go_out=internal/observer \
  --go_opt=module=github.com/sarattha/relayna-cell/internal/observer \
  --go_opt=Mcommon.proto=github.com/sarattha/relayna-cell/internal/observer/pb \
  --go_opt=Msyscall.proto=github.com/sarattha/relayna-cell/internal/observer/pb \
  --go_opt=Mcontainer.proto=github.com/sarattha/relayna-cell/internal/observer/pb \
  --go_opt=Msentry.proto=github.com/sarattha/relayna-cell/internal/observer/pb \
  internal/observer/proto/*.proto
