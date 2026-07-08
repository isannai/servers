# market — asset registry server. Multi-stage: compile the static binary in a
# build stage, ship it on a tiny distroless base. `docker compose up -d --build`
# builds everything inside — no host Go toolchain needed.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X github.com/isannai/isann-servers/pkg/setup.MarketVersion=${VERSION}" \
    -o /out/market ./cmd/market

FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/market /app/market
EXPOSE 8820
ENTRYPOINT ["/app/market"]
CMD ["--config", "/app/conf/market.json"]
