# syntax=docker/dockerfile:1

FROM golang:1.26.3-bookworm AS wasm-build

WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=js GOARCH=wasm GOWASM=satconv,signext \
    go build -mod=vendor -trimpath -o /out/main.wasm ./wasm

FROM node:24-bookworm-slim AS web-build

WORKDIR /src/lab
COPY lab/package.json lab/package-lock.json ./
RUN npm ci
COPY lab/ ./
COPY stream/ ../stream/
RUN mkdir -p public/sim public/wasm
COPY --from=wasm-build /out/main.wasm public/sim/main.wasm
COPY wasm/wasm_exec.js wasm/worker.js public/sim/
COPY wasm/index.html wasm/wasm_exec.js wasm/worker.js public/wasm/
RUN set -eu; \
    wasm_hash="$(sha256sum public/sim/main.wasm | cut -c1-16)"; \
    wasm_name="main.${wasm_hash}.wasm"; \
    mv public/sim/main.wasm "public/sim/${wasm_name}"; \
    sed -i "s|fetch(\"main.wasm\")|fetch(\"/sim/${wasm_name}\")|" \
        public/sim/worker.js public/wasm/worker.js; \
    grep -F "fetch(\"/sim/${wasm_name}\")" \
        public/sim/worker.js public/wasm/worker.js; \
    npm run build; \
    ln -s "${wasm_name}" dist/sim/main.wasm

FROM caddy:2.10-alpine

COPY Caddyfile /etc/caddy/Caddyfile
COPY --from=web-build /src/lab/dist /srv

EXPOSE 8080
