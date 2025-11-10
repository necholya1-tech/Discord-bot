# builder
FROM golang:1.24.4 AS builder
WORKDIR /app
ENV GOTOOLCHAIN=auto

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o app .

# runtime (DEBUG)
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/app /app/app
ENTRYPOINT ["/app/app"]


