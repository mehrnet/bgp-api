FROM --platform=$BUILDPLATFORM golang:1.22-bookworm AS build

ARG TARGETOS
ARG TARGETARCH
ARG RELEASE_TAG
ARG SOURCE_COMMIT
ARG BUILT_AT

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/bgp-api ./cmd/bgp-api
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
  -ldflags="-s -w -X main.version=${RELEASE_TAG} -X main.commit=${SOURCE_COMMIT} -X main.builtAt=${BUILT_AT}" \
  -o /out/bgp-api ./cmd/bgp-api

FROM scratch
COPY --from=build /out/bgp-api /bgp-api
USER 65532:65532
ENTRYPOINT ["/bgp-api"]
