COVERAGE_PROFILE := coverage.out
COVERAGE_THRESHOLD := 70

.PHONY: test coverage coverage-check coverage-report fuzz-request fuzz-response lint

test:
	go test ./...

lint:
	golangci-lint run ./...

coverage:
	@go test -coverprofile=$(COVERAGE_PROFILE) ./...

coverage-check: coverage
	@awk -v threshold="$(COVERAGE_THRESHOLD)" 'NR > 1 { total += $$2; if ($$3 > 0) covered += $$2 } END { pct = total ? 100 * covered / total : 0; printf "Total coverage: %.1f%%\n", pct; if (pct < threshold) { printf "FAIL: coverage %.1f%% is below threshold %s%%\n", pct, threshold; exit 1 } }' $(COVERAGE_PROFILE)

coverage-report: coverage
	@awk 'NR > 1 { split($$1, parts, ":"); file=parts[1]; stmts[file]+=$$2; if ($$3 > 0) covered[file]+=$$2 } END { for (file in stmts) printf "%.1f%% %s\n", 100 * covered[file] / stmts[file], file }' $(COVERAGE_PROFILE) | sort -n

fuzz-request:
	go test -fuzz=FuzzMcpRequestHandler -parallel=4

fuzz-response:
	go test -fuzz=FuzzMcpResponseHandler -parallel=4
