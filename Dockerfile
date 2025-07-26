# Stage 1: Build the Go application
FROM golang:latest AS builder

WORKDIR /app

# Copy go.mod and go.sum first to leverage Docker's caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the application source code
COPY . .

# Build the Go application
# CGO_ENABLED=0 disables CGO, creating a statically linked binary for better portability
# GOOS=linux ensures the binary is built for a Linux environment
# -a -ldflags='-s -w' reduces the binary size by stripping debug info and symbol table
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags='-s -w' -o yt_dl ./main.go #Adjust path to your main entry point

# Stage 2: Create the final lean image
FROM alpine:latest

# Install ca-certificates for HTTPS support if your application makes external calls
RUN apk add --no-cache ca-certificates

WORKDIR /app

# Copy the built binary from the builder stage
COPY --from=builder /app/yt_dl .

# Expose the port your Go application listens on (e.g., for a web server)
EXPOSE 9191

# Command to run the application when the container starts
CMD ["/yt_dl"]