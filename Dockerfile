# Build stage: compile a static binary. Migrations are embedded via go:embed.
FROM golang:1.24.13-alpine AS build
WORKDIR /src

# Dependencies first so this layer is cached when only source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags="-s -w" -o /out/seatlock .

# Runtime stage: no shell or package manager, runs as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/seatlock /seatlock
USER nonroot:nonroot
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/seatlock"]
