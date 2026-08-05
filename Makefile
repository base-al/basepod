.PHONY: ui build test dev

# ui builds the dashboard into web/dist. Only the placeholder
# web/dist/index.html is committed to the repo (see .gitignore) — this
# target overwrites the whole directory with a real build locally and in
# CI. Never commit the output of `make ui`.
ui:
	cd web && npm ci && npm run build

# build rebuilds the dashboard, then compiles it into the basepod binary
# via web/embed.go's go:embed.
build: ui
	CGO_ENABLED=0 go build -o basepod ./cmd/basepod

test:
	go test ./...

# dev runs the control plane and the dashboard separately so dashboard
# edits hot-reload: start the server in one terminal, then run the Vite
# dev server in another with BASEPOD_DEV_UI pointed at it so the server
# proxies / to Vite instead of serving the embedded build.
dev:
	@echo "Run in two terminals:"
	@echo "  1) go run ./cmd/basepod server --config <path-to-config>"
	@echo "  2) cd web && npm run dev   # then set BASEPOD_DEV_UI=http://localhost:5173 on (1)"
