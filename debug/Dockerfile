FROM golang:1.18-alpine as build

ENV GOPROXY direct

# Configure workspace
WORKDIR /workspace

# Install dlv
RUN apk update && apk add --no-cache \
    git && \
    CGO_ENABLED=0 go install github.com/go-delve/delve/cmd/dlv@latest

# Copy modules manifests
COPY ../go.mod go.mod
COPY ../go.sum go.sum

# Cache modules
RUN go mod download

COPY ../cmd cmd/
COPY ../pkg pkg/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags=aws -gcflags="all=-N -l" -o /controller cmd/controller/main.go

FROM alpine:3.16

ARG CLUSTER_NAME
ARG CLUSTER_ENDPOINT

COPY --from=build /controller /usr/local/bin
COPY --from=build /go/bin/dlv /usr/local/bin

ENTRYPOINT ["dlv", "--listen=:4000", "--headless=true", "--api-version=2", "--accept-multiclient", "exec", "/usr/local/bin/controller"]