BINARY := openshell-middleware

build:
	go build -o $(BINARY) ./cmd/openshell-middleware

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)

.PHONY: build test vet clean
