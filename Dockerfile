FROM golang:1.26.5-alpine AS build

WORKDIR /src
RUN apk add --no-cache jq
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/homelab-controller ./cmd/homelab-controller
RUN wget -qO- https://api.github.com/meta \
    | jq -r '.ssh_keys[] | "github.com \(.)"' \
    > /out/ssh_known_hosts \
    && test -s /out/ssh_known_hosts

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/homelab-controller /homelab-controller
COPY --from=build /out/ssh_known_hosts /etc/ssh/ssh_known_hosts
EXPOSE 8080
ENTRYPOINT ["/homelab-controller"]
