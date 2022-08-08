FROM golang:1.18-alpine as build

ENV GOPROXY direct
ENV CGO_ENABLED 0

# Configure workspace
WORKDIR /workspace

# Install dlv
RUN apk update && apk add --no-cache \
    git && \
    go install github.com/go-delve/delve/cmd/dlv@latest

# Copy modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# Cache modules
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/
RUN GOOS=linux GOARCH=amd64 go build -tags=aws -gcflags="all=-N -l" -ldflags="-w -s" -o /controller cmd/controller/main.go

FROM alpine:3.16

COPY --from=build /controller /usr/local/bin
COPY --from=build /go/bin/dlv /usr/local/bin

ENTRYPOINT ["dlv", "--listen:=4000", "--headless=true", "--api-version=2", "--accept-multiclient", "exec", "controller", "--cluster-name=joinnis-us-west-2", "--cluster-endpoint=https://ED19D5C48464207293DF47093ED07FBD.gr7.us-west-2.eks.amazonaws.com"]