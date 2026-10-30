# GO_BINARIES selects how the Go binaries reach the image. source (the default)
# builds them in this file. prebuilt copies them from the named build context
# prebuilt-binaries, which holds one linux-<arch>/ directory for each arch.
ARG GO_BINARIES=source

# Stage 1: Build the auth plugin HPI
FROM maven:3.9-eclipse-temurin-21 AS plugin-builder
WORKDIR /build
COPY plugin/pom.xml .
RUN mvn dependency:go-offline -B || true
COPY plugin/src/ src/
RUN mvn verify -DskipTests -B

# Stage 2: Go binaries, built from source
FROM golang:1.26-alpine AS binaries-source

RUN apk add --no-cache ca-certificates git

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build arguments change on every commit. Declare them after the dependency layers, so those layers stay cached.
ARG GIT_SHA=unknown
ARG GIT_BRANCH=unknown

# hack/build-image-binaries.sh holds the one binary list for both paths.
RUN GIT_SHA="${GIT_SHA}" sh hack/build-image-binaries.sh /build/bin

# Go binaries, copied from the prebuilt-binaries build context
FROM scratch AS binaries-prebuilt
ARG TARGETARCH
COPY --from=prebuilt-binaries /linux-${TARGETARCH}/ /build/bin/

FROM binaries-${GO_BINARIES} AS binaries

# Stage 3: Runtime
FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata git openssh-client && \
    addgroup -g 1000 jenkins && \
    adduser -u 1000 -G jenkins -D jenkins

COPY --from=binaries /build/bin/varroa-mite /app/varroa-mite
COPY --from=binaries /build/bin/varroa-gateway /app/varroa-gateway
COPY --from=binaries /build/bin/varroa-bff /app/varroa-bff
COPY --from=binaries /build/bin/varroa-operator /app/varroa-operator
COPY --from=binaries /build/bin/varroa-updatecenter /app/varroa-updatecenter
COPY --from=binaries /build/bin/varroa-recorder /app/varroa-recorder
COPY --from=binaries /build/bin/varroa-agent /app/varroa-agent
COPY --from=binaries /build/bin/varroactl /app/varroactl

# Bake the auth plugin HPI into the image for init-container delivery.
COPY --from=plugin-builder /build/target/varroa-mite-auth.hpi /opt/varroa/varroa-mite-auth.hpi

# Run as UID 1000 to match the Jenkins container (jenkins/jenkins image).
# This ensures both containers share the same file ownership on the PVC.
USER jenkins

EXPOSE 8080 9090
