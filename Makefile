BINARY := hivewire
GO     := go
# Extra arguments appended to every deploy target, e.g. make deploy ARGS="--port 9000"
ARGS   :=

.PHONY: all build clean test vet fmt deploy deploy-tui deploy-gui

all: build

## build: compile the binary into ./hivewire, overwriting any existing one
build:
	$(GO) build -o $(BINARY) .

## test: run the unit tests
test:
	$(GO) test ./...

## vet: run go vet
vet:
	$(GO) vet ./...

## fmt: format all Go sources
fmt:
	$(GO) fmt ./...

## clean: remove the built binary
clean:
	rm -f $(BINARY)

## deploy: build and run with both the TUI and the web UI (default behaviour)
deploy: build
	./$(BINARY) $(ARGS)

## deploy-tui: build and run the terminal UI only (web server disabled)
deploy-tui: build
	./$(BINARY) --web=false $(ARGS)

## deploy-gui: build and run the web UI only (terminal UI disabled)
deploy-gui: build
	./$(BINARY) --tui=false $(ARGS)
