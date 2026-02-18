FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o demo-broker .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /build/demo-broker /usr/local/bin/demo-broker
USER 65534:65534
ENTRYPOINT ["demo-broker"]
