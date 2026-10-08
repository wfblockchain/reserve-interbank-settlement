# Clearing-house settlement model — build/test helpers.
.PHONY: help test gotest economics hub hub-e2e all

help:
	@echo "  make test        forge test (contracts, via Docker)"
	@echo "  make gotest      go test ./... (optimiser, simulation, contract wiring)"
	@echo "  make economics   print the efficiency / accrual tables"
	@echo "  make hub         build the payment hub and portal (demo build)"
	@echo "  make hub-e2e     the payment hub's end-to-end business week (needs anvil)"
	@echo "  make all         everything above"

test:
	cd contracts && docker run --rm -v "$$PWD":/w -w /w ghcr.io/foundry-rs/foundry:stable "forge test"

gotest:
	go test ./... -count=1

economics:
	go run ./cmd/clearing-operator

hub:
	cd services/payments-svc && $(MAKE) build-demo

hub-e2e:
	cd services/payments-svc && $(MAKE) e2e

all: gotest test economics
