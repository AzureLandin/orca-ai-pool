TARGET := jailbreak-inject.so
INSTALL_DIR ?= ../../cliproxy/plugins/linux/amd64

build:
	CGO_ENABLED=1 go build -buildmode=c-shared -o $(TARGET) .

install: build
	mkdir -p $(INSTALL_DIR)
	cp $(TARGET) $(INSTALL_DIR)/

test:
	CGO_ENABLED=1 go test ./...

clean:
	rm -f $(TARGET) jailbreak-inject.h
