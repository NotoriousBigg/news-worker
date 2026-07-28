# ==========================================
# Stage 1: Build the Go binary
# ==========================================
FROM golang:1.25-alpine AS builder

# Set the working directory inside the container
WORKDIR /app

# Copy all source files into the container
COPY . .

# Fetch dependencies and generate go.sum automatically if it is missing
RUN go mod tidy

# Build a statically linked binary. 
# CGO_ENABLED=0 ensures it runs perfectly on the minimal Alpine image.
RUN CGO_ENABLED=0 GOOS=linux go build -o tracker-daemon main.go

# ==========================================
# Stage 2: Minimal Production Image
# ==========================================
FROM alpine:latest

# Install ca-certificates so the daemon can make secure HTTPS requests to the Telegram API
RUN apk --no-cache add ca-certificates

# Set the working directory for the runtime container
WORKDIR /root/

# Copy only the compiled binary from the builder stage
COPY --from=builder /app/tracker-daemon .

# Run the background worker
CMD ["./tracker-daemon"]
