FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /hub ./cmd/hub

# distroless/static ships CA certificates; timezones are embedded in the binary.
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /hub /app/hub
EXPOSE 8095
ENTRYPOINT ["/app/hub"]
