test:
	./build.sh
	cd server && go vet -tags sqlite_fts5 ./... && go test -tags sqlite_fts5 ./...

# The Linux/Windows backend (tesseract, poppler, whisper.cpp, Ollama) with the
# pure-Go SQLite, run on this machine: needs tesseract and poppler installed.
test-portable:
	cd server && CGO_ENABLED=0 go vet -tags portable ./... && CGO_ENABLED=0 go test -tags portable ./...

# Pure-Go server binaries for every platform, into dist/ (no C compiler needed).
cross:
	mkdir -p dist
	for t in linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		tags=; [ $$os = darwin ] && tags=portable; \
		(cd server && GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -tags "$$tags" -trimpath -ldflags="-s -w" \
			-o ../dist/supersearch-server-$$os-$$arch$$ext .) || exit 1; \
	done

install:
	./install.sh $(if $(VAULT),"$(VAULT)")

.PHONY: test test-portable cross install
