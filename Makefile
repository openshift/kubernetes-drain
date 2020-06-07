
all: deps-update

.PHONY: deps-update
deps-update:
	go mod tidy && \
	go mod vendor
