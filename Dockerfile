# syntax=docker/dockerfile:1
# Built by .github/workflows/build.yml for linux/arm64 (the cluster is Ampere A1).
# Local multi-arch: docker buildx build --platform linux/arm64,linux/amd64 -t switchboard:dev .
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/switchboard .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/switchboard /switchboard
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/switchboard"]
