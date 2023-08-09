all: binary

.PHONY: binary
binary: clean
	@echo "PHASE: Building qat-device-plugin ... "
	GOOS=linux go build -o qat_plugin ./cmd/qat_plugin.go

.PHONY: clean
clean:
	@echo 'PHASE: Cleaning ...'
	rm -rf _output &>/dev/null

.PHONY:
lint: