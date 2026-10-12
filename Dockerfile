FROM golang:1.24.2-alpine AS builder
WORKDIR /app

# Copy go module manifests and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY . .

# Build the binary from the entrypoint in cmd/server/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o server cmd/server/main.go

FROM golang:1.24.2-alpine
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/server .

EXPOSE 3000

# Run the server
ENTRYPOINT ["./server"]

