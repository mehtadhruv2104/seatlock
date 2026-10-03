BASE_URL ?= http://localhost:8080
ARGS ?=

.PHONY: up down test test-integration burst

# Run the service and Postgres locally (http://localhost:8080).
up:
	docker compose up --build -d

down:
	docker compose down -v

test:
	go test ./...

# Concurrency tests against a real, throwaway Postgres (needs Docker).
test-integration:
	./scripts/test-integration.sh

# On-sale stampede + correctness checks, e.g.
#   make burst BASE_URL=https://seatlock-production-4dd4.up.railway.app
#   make burst ARGS="-requests 5000 -concurrency 200"
burst:
	./burst.sh $(BASE_URL) $(ARGS)
