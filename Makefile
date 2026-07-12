# dbferry — developer tasks.
#
# The integration stand (poc-plan 0.2) is the main thing here for now: real
# PostgreSQL 14/17, MySQL 8 and a MinIO bucket, plus a throwaway age identity,
# all brought up with a single `make stand-up`.

STAND_DIR   := test/integration
COMPOSE     := docker compose -f $(STAND_DIR)/compose.yaml
STAND_STATE := $(STAND_DIR)/.stand

.PHONY: stand-up stand-down stand-ps stand-logs age-identity build test test-race test-integration test-fault cover vet fmt

## stand-up: generate the age identity (if missing) and start every backend, waiting until healthy
stand-up: age-identity
	# Wait only on the long-lived services (DBs gate on their healthchecks).
	# `--wait` treats any container that exits mid-wait as a failure, so the
	# one-shot bucket job is kept out of it and run synchronously below; its
	# own exit code (0) then gates readiness of MinIO + the bucket.
	$(COMPOSE) up -d --wait pg14 pg17 mysql8 minio
	$(COMPOSE) run --rm createbucket
	@echo
	@echo "stand is up:"
	@echo "  postgres 14   localhost:5414  (user=dbferry pass=dbferry db=postgres)"
	@echo "  postgres 17   localhost:5417  (user=dbferry pass=dbferry db=postgres)"
	@echo "  mysql 8       localhost:3308  (user=root    pass=dbferry db=dbferry)"
	@echo "  minio S3      localhost:9000  console localhost:9001 (minioadmin/minioadmin)"
	@echo "  bucket        s3://dbferry-backups"
	@echo "  age recipient $$(cat $(STAND_STATE)/age-recipient.txt)"

## stand-down: stop every backend and drop its (tmpfs) data
stand-down:
	$(COMPOSE) down -v

## stand-ps: show stand container status
stand-ps:
	$(COMPOSE) ps

## stand-logs: follow stand logs
stand-logs:
	$(COMPOSE) logs -f

## age-identity: generate a throwaway test age identity into $(STAND_STATE) (no-op if it exists)
age-identity:
	@go run ./$(STAND_DIR)/agekeygen -out $(STAND_STATE)

build:
	go build ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

## test: unit suite — fast, no external services
test:
	go test ./...

## test-race: unit suite under the race detector
test-race:
	go test -race ./...

## test-integration: full backup→restore→compare + CLI (named connections) against the stand (needs `make stand-up`)
test-integration:
	go test -tags=integration ./...

## test-fault: fault-injection suite against the stand (needs `make stand-up`)
test-fault:
	go test -tags=faultinjection ./...

## cover: coverage of own packages across all suites, enforced against the threshold (needs the stand)
cover:
	./scripts/coverage.sh
