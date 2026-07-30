# List available recipes
default:
    @just --list

# Build the exporter binary
build:
    go build -o vsphere-exporter .

# Vet and run all tests (in-process vCenter simulator, no external deps)
test:
    go vet ./...
    go test ./...

# Format the code
fmt:
    go fmt ./...

# Tidy the module dependencies
tidy:
    go mod tidy

# Run the exporter against the in-process simulator
run-mock:
    go run . -mocking -interval 5

# Fetch the vcenter metrics from a running exporter
metrics:
    curl -s http://localhost:2112/metrics | grep -E '^vcenter_'
