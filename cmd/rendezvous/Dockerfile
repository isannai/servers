# rendezvous (rv) — P2P NAT hole-punch coordination. Multi-stage: build stage
# is golang (has the Go toolchain), runtime is a tiny distroless base.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X github.com/isannai/isann-servers/pkg/setup.RendezvousVersion=${VERSION}" \
    -o /out/rendezvous ./cmd/rendezvous

FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/rendezvous /app/rendezvous
EXPOSE 9100/tcp
EXPOSE 9100/udp
EXPOSE 9000/tcp
ENTRYPOINT ["/app/rendezvous"]
CMD ["--config", "/app/conf/rendezvous.json"]
