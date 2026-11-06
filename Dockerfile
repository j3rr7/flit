FROM golang:1.27.3-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /bin/flit ./cmd/flit

FROM alpine:3.9.6

RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /bin/flit /app/flit

EXPOSE 8080
ENTRYPOINT ["/app/flit"]
