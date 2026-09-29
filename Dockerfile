FROM golang:1.26-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN GOMAXPROCS=1 CGO_ENABLED=0 go build -p=1 -trimpath -ldflags='-s -w' -o /out/modelgate ./cmd/modelgate \
 && GOMAXPROCS=1 CGO_ENABLED=0 go build -p=1 -trimpath -ldflags='-s -w' -o /out/llm-mock ./cmd/llm-mock

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN apk add --no-cache ca-certificates \
 && addgroup -S modelgate \
 && adduser -S -G modelgate -h /nonexistent -s /sbin/nologin modelgate
COPY --from=build /out/modelgate /usr/local/bin/modelgate
COPY --from=build /out/llm-mock /usr/local/bin/llm-mock
USER modelgate:modelgate
ENTRYPOINT ["/usr/local/bin/modelgate"]
CMD ["serve"]
