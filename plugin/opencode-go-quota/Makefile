TARGET := opencode-go-quota.so
INSTALL_DIR ?= ../../cliproxy/plugins/linux/amd64

build:
	CGO_ENABLED=1 go build -buildmode=c-shared -o $(TARGET) .

install: build
	mkdir -p $(INSTALL_DIR)
	cp $(TARGET) $(INSTALL_DIR)/

test:
	go vet ./...

clean:
	rm -f $(TARGET) opencode-go-quota.h
