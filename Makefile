test:
	./build-helper.sh
	cd server && go vet ./... && go test ./...

install:
	./install.sh $(if $(VAULT),"$(VAULT)")

.PHONY: test install
