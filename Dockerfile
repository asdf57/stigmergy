FROM golang:1.26.5-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/homelab-controller ./cmd/homelab-controller

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/homelab-controller /homelab-controller
EXPOSE 8080
ENTRYPOINT ["/homelab-controller"]
