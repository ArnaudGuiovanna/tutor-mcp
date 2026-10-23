FROM golang:1.26.10 AS build
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
RUN apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/*
RUN python3 scripts/third_party_notices.py --binary /out/tutor-mcp \
    --output /out/THIRD_PARTY_NOTICES --tags timetzdata --ca-certificates
RUN cp LICENSE /out/LICENSE
RUN mkdir /data && chmod 0700 /data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/tutor-mcp /usr/local/bin/tutor-mcp
COPY --from=build /out/LICENSE /usr/share/doc/tutor-mcp/LICENSE
COPY --from=build /out/THIRD_PARTY_NOTICES /usr/share/doc/tutor-mcp/THIRD_PARTY_NOTICES
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
ENV HOME=/data
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/tutor-mcp"]
CMD ["--profile", "hobby", "--data-dir", "/data/hobby"]
