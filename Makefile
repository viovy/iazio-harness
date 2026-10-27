.PHONY: all test test-e2e-mock-agy

all: test

test:
	go test ./...

test-e2e-mock-agy:
	CGO_ENABLED=1 go test -v -race -count=1 -run "TestE2E_" ./...
