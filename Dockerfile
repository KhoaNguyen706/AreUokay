FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/core ./cmd/core && CGO_ENABLED=0 go build -o /out/replay ./cmd/replay

FROM alpine:3.20
COPY --from=build /out/ /usr/local/bin/
EXPOSE 8080
CMD ["core"]
