BINARY := openshell-middleware

build:
	go build -o $(BINARY) ./cmd/openshell-middleware

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)

# OpenShell's protos carry no go_package, so both are mapped into pkg/openshell.
# The vendored copies come from the OpenShell release named in UPSTREAM_VERSION.
PROTO_MAP := Msupervisor_middleware.proto=github.com/armoriq/armoriq-openshell-middleware/pkg/openshell,Mextension.proto=github.com/armoriq/armoriq-openshell-middleware/pkg/openshell

proto:
	protoc -I proto/openshell \
		--go_out=pkg/openshell --go_opt=paths=source_relative,$(PROTO_MAP) \
		--go-grpc_out=pkg/openshell --go-grpc_opt=paths=source_relative,$(PROTO_MAP) \
		supervisor_middleware.proto extension.proto

.PHONY: build test vet clean proto
