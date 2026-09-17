FROM golang:1.26.8 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -tags timetzdata \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
    -o /out/tutor-mcp .
RUN mkdir /data && chmod 0700 /data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/tutor-mcp /usr/local/bin/tutor-mcp
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
ENV HOME=/data
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/tutor-mcp"]
CMD ["--profile", "hobby", "--data-dir", "/data/hobby"]
