# Stage 1: Build the auth plugin HPI
FROM maven:3.9-eclipse-temurin-21 AS plugin-builder
WORKDIR /build
COPY plugin/pom.xml .
RUN mvn dependency:go-offline -B || true
COPY plugin/src/ src/
RUN mvn package -DskipTests -B

# Stage 2: Build Go binaries
FROM golang:1.26-alpine AS builder

ARG GIT_SHA=unknown
ARG GIT_BRANCH=unknown

RUN apk add --no-cache ca-certificates git

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w" \
    -o /build/bin/varroa-mite \
    ./cmd/mite/ && \
    CGO_ENABLED=0 go build \
    -ldflags="-s -w" \
    -o /build/bin/varroa-gateway \
    ./cmd/gateway/ && \
    CGO_ENABLED=0 go build \
    -ldflags="-s -w" \
    -o /build/bin/varroa-bff \
    ./cmd/bff/ && \
    CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.version=${GIT_SHA}" \
    -o /build/bin/varroa-operator \
    ./cmd/operator/ && \
    CGO_ENABLED=0 go build \
    -ldflags="-s -w" \
    -o /build/bin/varroa-updatecenter \
    ./cmd/updatecenter/ && \
    CGO_ENABLED=0 go build \
    -ldflags="-s -w" \
    -o /build/bin/varroactl \
    ./cmd/varroactl/

# Stage 3: Runtime
FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata git openssh-client && \
    addgroup -g 1000 jenkins && \
    adduser -u 1000 -G jenkins -D jenkins

COPY --from=builder /build/bin/varroa-mite /app/varroa-mite
COPY --from=builder /build/bin/varroa-gateway /app/varroa-gateway
COPY --from=builder /build/bin/varroa-bff /app/varroa-bff
COPY --from=builder /build/bin/varroa-operator /app/varroa-operator
COPY --from=builder /build/bin/varroa-updatecenter /app/varroa-updatecenter
COPY --from=builder /build/bin/varroactl /app/varroactl

# Bake the auth plugin HPI into the image for init-container delivery.
COPY --from=plugin-builder /build/target/varroa-mite-auth.hpi /opt/varroa/varroa-mite-auth.hpi

# Run as UID 1000 to match the Jenkins container (jenkins/jenkins image).
# This ensures both containers share the same file ownership on the PVC.
USER jenkins

EXPOSE 8080 9090
