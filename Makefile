PDFJS := 4.10.38
SORTABLE := 1.15.6
VENDOR := web/static/vendor

.PHONY: build run vendor docker up logs

vendor:
	curl -fsSL -o $(VENDOR)/pdf.min.mjs        https://cdn.jsdelivr.net/npm/pdfjs-dist@$(PDFJS)/build/pdf.min.mjs
	curl -fsSL -o $(VENDOR)/pdf.worker.min.mjs https://cdn.jsdelivr.net/npm/pdfjs-dist@$(PDFJS)/build/pdf.worker.min.mjs
	curl -fsSL -o $(VENDOR)/Sortable.min.js    https://cdn.jsdelivr.net/npm/sortablejs@$(SORTABLE)/Sortable.min.js

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o aio ./cmd/aio

run: build
	AIO_DATA_DIR=./data AIO_PLUGIN_DIRS=./plugins ./aio

docker:
	docker compose build

up:
	docker compose up -d --build

logs:
	docker compose logs -f
