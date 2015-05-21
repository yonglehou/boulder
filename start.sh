#!/bin/bash
# Run both boulder and cfssl, using test configs.
if type realpath >/dev/null 2>/dev/null; then
  cd $(realpath $(dirname $0))
fi

# Kill all children on exit.
exec go run ./cmd/boulder/main.go --config test/boulder-config.json
