FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/hospital ./cmd/hospital \
    && CGO_ENABLED=0 go build -o /out/surgeon ./cmd/surgeon

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 hospital \
    && useradd --uid 65532 --gid 65532 --create-home --shell /usr/sbin/nologin hospital
COPY --from=build /out/hospital /usr/local/bin/hospital
COPY --from=build /out/surgeon /usr/local/bin/surgeon
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/hospital"]
