# rs-relay: pairs studios and musicians by code; sees only ciphertext.
# Build from the repository root:
#   docker build -f deploy/relay/Dockerfile -t rs-relay .
#   docker run -d --restart=always -p 7401:7401 rs-relay
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /rs-relay ./cmd/rs-relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /rs-relay /rs-relay
EXPOSE 7401
ENTRYPOINT ["/rs-relay"]
