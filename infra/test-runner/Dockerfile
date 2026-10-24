# Go test runner for scripts/integration-test.sh (docker-compose.test.yml).
# python3 is baked in: Go integration tests exec the served monitor runner
# (sk-579) and t.Skip without it. Installing it per run (apk add at start)
# made every run depend on the Alpine mirror being reachable; built once, the
# layer is cached.
FROM golang:1.26-alpine
RUN apk add --no-cache python3
