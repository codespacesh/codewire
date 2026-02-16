# Build stage
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o cw ./cmd/cw

# Runtime stage
FROM alpine:3.19

RUN apk add --no-cache bash ca-certificates

COPY --from=builder /app/cw /usr/local/bin/cw

EXPOSE 9100

ENTRYPOINT ["cw"]
CMD ["node"]
