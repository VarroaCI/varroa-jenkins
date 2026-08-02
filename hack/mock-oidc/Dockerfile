FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o mock-oidc .

FROM scratch
COPY --from=builder /build/mock-oidc /app/mock-oidc
EXPOSE 8080
ENTRYPOINT ["/app/mock-oidc"]
