test:
	./build-helper.sh
	cd server && go vet -tags sqlite_fts5 ./... && go test -tags sqlite_fts5 ./...

install:
	./install.sh $(if $(VAULT),"$(VAULT)")

.PHONY: test install
