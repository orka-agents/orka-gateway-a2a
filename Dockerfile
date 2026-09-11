# Build from this repository's root.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/a2a-gateway .

FROM gcr.io/distroless/static-debian12:nonroot
COPY LICENSE /LICENSE
COPY --from=build /out/a2a-gateway /a2a-gateway
EXPOSE 8443
ENTRYPOINT ["/a2a-gateway"]
CMD ["-config", "/etc/a2a/config.json"]
