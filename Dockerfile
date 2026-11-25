# Build a static binary, then copy it into an image that has no shell and
# no package manager.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /warren ./cmd/warren

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /warren /warren
USER 65532:65532
ENTRYPOINT ["/warren"]
